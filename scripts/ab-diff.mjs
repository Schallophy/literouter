import assert from "node:assert/strict";

const NODE = "http://127.0.0.1:8898";
const GO = "http://127.0.0.1:8899";

const failures = [];

function normalize(value) {
  if (Array.isArray(value)) return value.map(normalize);
  if (value && typeof value === "object") {
    const out = {};
    for (const [key, entry] of Object.entries(value)) {
      if (["created", "created_at", "bootedAt", "now", "fetchedAt", "at", "lastUsedAt", "firstSeenAt", "lastSeenAt", "checkedAt", "throttledUntil"].includes(key)) {
        out[key] = entry === null ? null : "TIME";
        continue;
      }
      if (key === "uptimeSeconds" || key === "latencyMs") {
        out[key] = entry === null ? null : 0;
        continue;
      }
      if (key === "owned_by") {
        out[key] = "BRAND";
        continue;
      }
      if (key === "id" && typeof entry === "string" && /^(chatcmpl-|resp_|msg_)\d+/.test(entry)) {
        out[key] = "ID";
        continue;
      }
      if ((key === "type" || key === "code") && typeof entry === "string") {
        out[key] = entry.replace(/^(switchyard|literouter)\./, "BRAND.");
        continue;
      }
      out[key] = normalize(entry);
    }
    return out;
  }
  if (typeof value === "string") {
    return value.replace(/switchyard|literouter/gi, "BRAND");
  }
  return value;
}

async function request(base, method, path, body, headers = {}) {
  const response = await fetch(`${base}${path}`, {
    method,
    headers: { ...(body === undefined ? {} : { "content-type": "application/json" }), ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  let parsed = null;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = text;
  }
  return {
    status: response.status,
    headers: {
      model: response.headers.get("x-switchyard-model") ?? response.headers.get("x-literouter-model"),
      provider: response.headers.get("x-switchyard-provider") ?? response.headers.get("x-literouter-provider"),
      failovers: response.headers.get("x-switchyard-failovers") ?? response.headers.get("x-literouter-failovers"),
      retryAfter: response.headers.get("retry-after"),
    },
    parsed,
    text,
  };
}

function normalizeSSE(text) {
  return text
    .split(/\n\n/)
    .map((block) => block.trim())
    .filter(Boolean)
    .map((block) => {
      const lines = block.split("\n");
      const data = lines.filter((line) => line.startsWith("data: ")).map((line) => line.slice(6)).join("\n");
      if (data === "[DONE]") return "[DONE]";
      try {
        return JSON.stringify(normalize(JSON.parse(data)));
      } catch {
        return block;
      }
    });
}

async function compare(name, scenario) {
  try {
    const [node, go] = await Promise.all([scenario(NODE), scenario(GO)]);
    assert.deepEqual(go.status, node.status);
    assert.deepEqual(go.headers, node.headers);
    if (scenario.sse) {
      assert.deepEqual(normalizeSSE(go.text), normalizeSSE(node.text));
    } else {
      assert.deepEqual(normalize(go.parsed), normalize(node.parsed));
    }
    process.stdout.write(`PASS ${name}\n`);
  } catch (error) {
    failures.push(name);
    process.stdout.write(`DIFF ${name}: ${error.message}\n`);
  }
}

const chat = (model, extra = {}) => ({ model, messages: [{ role: "user", content: "hi" }], ...extra });

await compare("v1/models", (base) => request(base, "GET", "/v1/models"));

await compare("chat auto 403->429->ok", (base) => request(base, "POST", "/v1/chat/completions", chat("auto")));

await compare("chat auto after cooldown", (base) => request(base, "POST", "/v1/chat/completions", chat("auto")));

await compare("bare model name", (base) => request(base, "POST", "/v1/chat/completions", chat("qwen3.8-27b")));

{
  const node = await request(NODE, "POST", "/v1/chat/completions", chat("auto", { stream: true }));
  const go = await request(GO, "POST", "/v1/chat/completions", chat("auto", { stream: true }));
  try {
    assert.deepEqual(go.status, node.status);
    assert.deepEqual(go.headers, node.headers);
    assert.deepEqual(normalizeSSE(go.text), normalizeSSE(node.text));
    process.stdout.write("PASS chat stream\n");
  } catch (error) {
    failures.push("chat stream");
    process.stdout.write(`DIFF chat stream: ${error.message}\n`);
  }
}

await compare("context overflow", (base) =>
  request(base, "POST", "/v1/chat/completions", chat("qwen3.8-27b", { max_tokens: 300_000 })),
);

let nodeResponseID = null;
let goResponseID = null;
{
  const body = {
    model: "qwen3.8-27b",
    input: [{ role: "user", content: "hello" }],
  };
  const nodeFirst = await request(NODE, "POST", "/v1/responses", body);
  const goFirst = await request(GO, "POST", "/v1/responses", body);
  nodeResponseID = nodeFirst.parsed?.id;
  goResponseID = goFirst.parsed?.id;
  try {
    assert.deepEqual(goFirst.status, nodeFirst.status);
    assert.deepEqual(normalize(goFirst.parsed), normalize(nodeFirst.parsed));
    process.stdout.write("PASS responses first turn\n");
  } catch (error) {
    failures.push("responses first turn");
    process.stdout.write(`DIFF responses first turn: ${error.message}\n`);
  }
}

{
  const input = [
    { role: "user", content: "hello" },
    { role: "assistant", content: "previous answer" },
    { role: "user", content: "again" },
  ];
  const nodeSecond = await request(NODE, "POST", "/v1/responses", {
    model: "qwen3.8-27b", input, previous_response_id: nodeResponseID,
  });
  const goSecond = await request(GO, "POST", "/v1/responses", {
    model: "qwen3.8-27b", input, previous_response_id: goResponseID,
  });
  try {
    assert.deepEqual(goSecond.status, nodeSecond.status);
    assert.deepEqual(normalize(goSecond.parsed), normalize(nodeSecond.parsed));
    process.stdout.write("PASS responses continuation\n");
  } catch (error) {
    failures.push("responses continuation");
    process.stdout.write(`DIFF responses continuation: ${error.message}\n`);
  }
}

await compare("embeddings", (base) =>
  request(base, "POST", "/v1/embeddings", { model: "qwen3.8-27b", input: "hello" }),
);

await compare("overview projection", async (base) => {
  const response = await request(base, "GET", "/api/overview");
  const data = response.parsed;
  return {
    status: response.status,
    parsed: {
      app: { name: data.app.name, host: data.app.host, port: 0 },
      key: data.key,
      stats: {
        requests: data.stats.requests,
        failovers: data.stats.failovers,
        promptTokens: data.stats.promptTokens,
        completionTokens: data.stats.completionTokens,
        cost: data.stats.cost,
      },
      chaining: data.chaining,
      output: data.output,
      chain: {
        effective: data.chain.effective,
        alive: data.chain.alive,
        dead: data.chain.dead,
        models: data.chain.models.map((model) => ({
          code: model.code, health: model.health, apis: model.apis, provider: model.provider,
        })),
      },
    },
  };
});

await compare("static index", async (base) => {
  const response = await fetch(`${base}/`);
  const text = await response.text();
  return {
    status: response.status,
    parsed: {
      cacheControl: response.headers.get("cache-control"),
      contentType: response.headers.get("content-type"),
      hasDashboard: text.includes('id="view"') || text.includes("app"),
    },
  };
});

if (failures.length > 0) {
  process.stdout.write(`FAILED: ${failures.join(", ")}\n`);
  process.exit(1);
}
process.stdout.write("ALL PASS\n");
