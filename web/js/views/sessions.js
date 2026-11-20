import { api } from "../api.js";
import { esc, fmtAgo, fmtDateTime, fmtInt } from "../format.js";
import { renderMarkdown } from "../markdown.js";
import { openDialog, toast } from "../ui.js";

const local = {
  sessions: [],
  agents: [],
  errors: [],
  loaded: false,
  loading: false,
  error: null,
  agent: "all",
  copying: new Set(),
  viewing: new Set(),
};

let rootEl = null;
let abort = null;
let mounted = false;

export function mount(root) {
  rootEl = root;
  mounted = true;
  local.sessions = [];
  local.agents = [];
  local.errors = [];
  local.loaded = false;
  local.loading = false;
  local.error = null;
  local.agent = "all";
  local.copying.clear();
  local.viewing.clear();

  abort = new AbortController();
  root.addEventListener("click", onClick, { signal: abort.signal });

  paint();
  load();
  return { update, destroy };
}

function destroy() {
  mounted = false;
  abort?.abort();
  abort = null;
  rootEl = null;
  local.copying.clear();
  local.viewing.clear();
}

async function load(refresh = false) {
  if (!mounted || local.loading) return;
  local.loading = true;
  paint();

  try {
    const data = await api.get(`/api/sessions${refresh ? "?refresh=1" : ""}`);
    local.sessions = Array.isArray(data?.sessions) ? data.sessions : [];
    local.agents = Array.isArray(data?.agents) ? data.agents : [];
    local.errors = Array.isArray(data?.errors) ? data.errors : [];
    local.error = null;
  } catch (error) {
    local.error = error.message;
  } finally {
    local.loading = false;
    local.loaded = true;
    paint();
  }
}

function paint() {
  if (!rootEl) return;

  rootEl.innerHTML = `
    <div class="page-head">
      <div>
        <h1 class="page-title">会话</h1>
        <p class="page-sub">汇总本机各编程助手的会话，按最近对话时间排序；复制时优先保留上次压缩摘要，并附上压缩后的完整对话</p>
      </div>
      <div class="page-tools">
        <button class="btn" type="button" data-action="refresh"${
          local.loading ? " disabled" : ""
        }>${local.loading ? "扫描中…" : "重新扫描"}</button>
      </div>
    </div>

    <div class="sessions-tools">
      <div class="chips" id="session-filters">
        <button class="chip" type="button" data-agent="all" aria-pressed="${
          local.agent === "all"
        }">全部</button>
        ${local.agents
          .map((agent) => {
            const count = local.sessions.filter((item) => item.agent === agent.id).length;
            return `<button class="chip" type="button" data-agent="${esc(agent.id)}" aria-pressed="${
              local.agent === agent.id
            }" title="${esc(agent.store || "")}"${agent.present ? "" : " disabled"}>${esc(
              agent.label,
            )}<span class="chip-count">${fmtInt(count)}</span></button>`;
          })
          .join("")}
      </div>
      <span class="muted-note" id="session-total">${local.loaded ? totalText() : ""}</span>
    </div>

    <div id="sessions-body">${bodyHtml()}</div>
  `;
}

function totalText() {
  const count = visibleSessions().length;
  return `${fmtInt(count)} 个会话`;
}

function paintBody() {
  const body = rootEl?.querySelector("#sessions-body");
  if (!body) return;
  body.innerHTML = bodyHtml();
  const total = rootEl?.querySelector("#session-total");
  if (total) total.textContent = local.loaded ? totalText() : "";
}

function bodyHtml() {
  if (!local.loaded) {
    return `<div class="sk-stack" aria-busy="true" aria-label="载入中" style="margin-top:20px">
      <div class="sk" style="height:44px"></div>
      <div class="sk" style="height:44px"></div>
      <div class="sk" style="height:44px"></div>
    </div>`;
  }

  if (local.error) {
    return `<div class="notice" data-tone="bad" style="margin-top:20px">
      <div>
        <p class="notice-title">读取会话失败</p>
        <p>${esc(local.error)}</p>
        <p style="margin-top:8px"><button class="link-btn" type="button" data-action="retry">重试</button></p>
      </div>
    </div>`;
  }

  const rows = visibleSessions();
  if (!rows.length) {
    const present = local.agents.filter((agent) => agent.present);
    return `<div class="empty">
      <p class="empty-title">没有找到会话</p>
      <p>${
        present.length
          ? `已检测到 ${present.map((agent) => esc(agent.label)).join("、")}，但这些助手还没有留下可读的会话记录。`
          : "没有检测到已安装的编程助手。装上 OpenCode、Qoder、WorkBuddy 或 ZCode 后再回来看看。"
      }</p>
    </div>`;
  }

  return `
    ${errorsHtml()}
    <div class="table-wrap">
      <table class="table sessions-table">
        <thead>
          <tr>
            <th>会话</th>
            <th class="col-agent">助手</th>
            <th class="col-updated">最近对话</th>
            <th class="col-cwd">目录</th>
            <th class="col-action"><span class="sr-only">操作</span></th>
          </tr>
        </thead>
        <tbody>${rows.map(rowHtml).join("")}</tbody>
      </table>
    </div>
  `;
}

function errorsHtml() {
  if (!local.errors.length) return "";
  return `<div class="notice" data-tone="hold" style="margin-bottom:16px">
    <div>
      <p class="notice-title">部分来源读取失败</p>
      ${local.errors.map((message) => `<p>${esc(message)}</p>`).join("")}
    </div>
  </div>`;
}

function visibleSessions() {
  const rows =
    local.agent === "all"
      ? local.sessions
      : local.sessions.filter((item) => item.agent === local.agent);
  return [...rows].sort((left, right) => timeOf(right) - timeOf(left));
}

function timeOf(session) {
  const at = Date.parse(session?.updated || "");
  return Number.isFinite(at) ? at : 0;
}

function agentLabel(agent) {
  return local.agents.find((item) => item.id === agent)?.label || agent;
}

function rowHtml(session) {
  const copying = local.copying.has(key(session));
  const updated = session.updated
    ? `<time datetime="${esc(session.updated)}" title="${esc(fmtDateTime(session.updated))}">${esc(
        fmtAgo(session.updated),
      )}</time>`
    : "<span class=\"muted-note\">未知</span>";
  const cwd = session.cwd
    ? `<span class="mono session-path" title="${esc(session.cwd)}">${esc(session.cwd)}</span>`
    : "<span class=\"muted-note\">—</span>";

  return `
    <tr>
      <td data-label="会话">
        <div class="session-title">${esc(session.title || session.id)}</div>
        <div class="session-meta mono" title="${esc(session.id)}">${esc(session.id)}</div>
      </td>
      <td class="col-agent" data-label="助手"><span class="session-agent">${esc(agentLabel(session.agent))}</span></td>
      <td class="col-updated" data-label="最近对话">${updated}</td>
      <td class="col-cwd" data-label="目录">${cwd}</td>
      <td class="col-action">
        <div class="session-actions">
          <button class="btn sm ghost" type="button" data-action="view" data-agent="${esc(
            session.agent,
          )}" data-id="${esc(session.id)}"${
            local.viewing.has(key(session)) ? " disabled" : ""
          }>${local.viewing.has(key(session)) ? "载入中…" : "查看"}</button>
          <button class="btn sm" type="button" data-action="copy" data-agent="${esc(
            session.agent,
          )}" data-id="${esc(session.id)}"${copying ? " disabled" : ""}>${
            copying ? "生成中…" : "复制交接"
          }</button>
        </div>
      </td>
    </tr>
  `;
}

function key(session) {
  return `${session.agent}\u0000${session.id}`;
}

function onClick(event) {
  const chip = event.target.closest(".chip[data-agent]");
  if (chip && !chip.disabled) {
    local.agent = chip.dataset.agent;
    for (const item of rootEl.querySelectorAll(".chip[data-agent]")) {
      item.setAttribute("aria-pressed", String(item === chip));
    }
    paintBody();
    return;
  }

  const target = event.target.closest("[data-action]");
  if (!target || target.disabled) return;

  const action = target.dataset.action;
  if (action === "refresh") {
    local.loading = false;
    load(true);
    return;
  }
  if (action === "retry") {
    local.loaded = false;
    local.error = null;
    paint();
    load();
    return;
  }
  if (action === "copy") {
    copy(target.dataset.agent, target.dataset.id, target);
    return;
  }
  if (action === "view") {
    view(target.dataset.agent, target.dataset.id, target);
  }
}

async function view(agent, id, button) {
  const token = key({ agent, id });
  if (local.viewing.has(token)) return;
  local.viewing.add(token);
  if (button) {
    button.disabled = true;
    button.textContent = "载入中…";
  }

  const dialog = openDialog({
    title: "会话详情",
    className: "session-dialog",
    body: `<div class="sk-stack" aria-busy="true" aria-label="载入会话" style="padding:20px">
      <div class="sk" style="height:26px"></div>
      <div class="sk" style="height:120px"></div>
      <div class="sk" style="height:120px"></div>
    </div>`,
    footer: `<button class="btn" type="button" data-copy disabled>复制交接</button>
      <button class="btn primary" type="button" data-dismiss>关闭</button>`,
  });

  const copyButton = dialog.querySelector("[data-copy]");
  dialog.querySelector("[data-dismiss]")?.addEventListener("click", () => dialog.close());
  copyButton?.addEventListener("click", () => copy(agent, id, copyButton));

  try {
    const detail = await api.get(
      `/api/sessions/detail?agent=${encodeURIComponent(agent)}&id=${encodeURIComponent(id)}`,
    );
    if (!dialog.isConnected) return;
    const title = dialog.querySelector(".dialog-title");
    if (title) title.textContent = detail?.title || detail?.id || "会话详情";
    const body = dialog.querySelector(".dialog-body");
    if (body) body.innerHTML = detailHtml(detail);
    if (copyButton) copyButton.disabled = false;
  } catch (error) {
    if (!dialog.isConnected) return;
    const body = dialog.querySelector(".dialog-body");
    if (body) {
      body.innerHTML = `<div class="notice" data-tone="bad">
        <div>
          <p class="notice-title">读取会话失败</p>
          <p>${esc(error.message || "未知错误")}</p>
        </div>
      </div>`;
    }
  } finally {
    local.viewing.delete(token);
    if (button?.isConnected) {
      button.disabled = false;
      button.textContent = "查看";
    }
  }
}

function detailHtml(detail) {
  const messages = Array.isArray(detail?.messages) ? detail.messages : [];
  const meta = [
    `<span class="session-agent">${esc(agentLabel(detail?.agent || ""))}</span>`,
    detail?.updated ? `<span>最近对话 ${esc(fmtDateTime(detail.updated))}</span>` : "",
    detail?.cwd ? `<span class="mono">${esc(detail.cwd)}</span>` : "",
    detail?.id ? `<span class="mono">${esc(detail.id)}</span>` : "",
  ]
    .filter(Boolean)
    .join("");

  const summary = detail?.summary
    ? `<details class="session-view-summary" open>
        <summary>上次压缩摘要${
          detail.summaryAt ? ` · ${esc(fmtDateTime(detail.summaryAt))}` : ""
        }</summary>
        <div class="session-msg-text">${renderMarkdown(detail.summary)}</div>
      </details>`
    : "";

  const note = `共 ${fmtInt(messages.length)} 条消息。`;

  return `<div class="session-view">
    <div class="session-view-meta">${meta}</div>
    <p class="session-view-note">${note}</p>
    ${summary}
    <div class="session-view-messages">${
      messages.length
        ? messages.map(messageHtml).join("")
        : `<p class="muted-note">没有可显示的消息。</p>`
    }</div>
  </div>`;
}

function messageHtml(item) {
  const role = String(item?.role || "助手");
  const label =
    role === "用户" ? "" : `<div class="session-msg-role">${esc(role)}</div>`;
  return `<div class="session-msg" data-role="${esc(role)}">
    ${label}
    <div class="session-msg-text">${renderMarkdown(item?.text || "")}</div>
  </div>`;
}

async function copy(agent, id, button) {
  const token = `${agent}\u0000${id}`;
  if (local.copying.has(token)) return;
  local.copying.add(token);
  button.disabled = true;
  button.textContent = "生成中…";

  try {
    const result = await api.get(
      `/api/sessions/handoff?agent=${encodeURIComponent(agent)}&id=${encodeURIComponent(id)}`,
    );
    const text = String(result?.text || "");
    if (!text.trim()) throw new Error("交接摘要为空");
    await writeClipboard(text);
    toast(
      `已复制「${result.title || id}」的交接摘要，约 ${fmtInt(result.chars)} 字`,
      "ok",
      "交接摘要",
    );
  } catch (error) {
    toast(error.message || "复制失败", "bad", "交接摘要");
  } finally {
    local.copying.delete(token);
    if (rootEl && button.isConnected) {
      button.disabled = false;
      button.textContent = "复制交接";
    }
  }
}

async function writeClipboard(text) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // 内嵌浏览器可能拒绝异步剪贴板写入，退回选中复制。
    }
  }
  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.opacity = "0";
  document.body.append(area);
  area.select();
  const ok = document.execCommand("copy");
  area.remove();
  if (!ok) throw new Error("浏览器拒绝了剪贴板写入");
}

function update() {
  // 会话页按需扫描，不跟随 overview 轮询刷新。
}