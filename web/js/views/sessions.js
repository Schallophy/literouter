import { api } from "../api.js";
import { esc, fmtAgo, fmtDateTime, fmtInt } from "../format.js";
import { renderMarkdown } from "../markdown.js";
import { toast } from "../ui.js";

const REASONS = [
  ["", "按检测结果"],
  ["quota", "额度用完"],
  ["upstream", "上游报错"],
  ["aborted", "被中断"],
  ["switch", "主动换人"],
  ["other", "其他原因"],
];

const choice = {
  reason: "",
  note: "",
  summary: true,
  messages: true,
  todos: true,
  full: false,
  limit: 40000,
};

const local = {
  all: [],
  stuckOnly: false,
  query: "",
  selected: null,
  detail: null,
  handoff: null,
  seq: 0,
};

let rootEl = null;
let abort = null;
let mounted = false;
let filterTimer = 0;
let noteTimer = 0;

export function mount(root) {
  rootEl = root;
  mounted = true;
  local.all = [];
  local.stuckOnly = false;
  local.query = "";
  local.selected = null;
  local.detail = null;
  local.handoff = null;
  local.seq = 0;
  choice.reason = "";
  choice.note = "";
  choice.summary = true;
  choice.messages = true;
  choice.todos = true;
  choice.full = false;
  choice.limit = 40000;

  abort = new AbortController();
  const options = { signal: abort.signal };
  root.addEventListener("click", onClick, options);
  root.addEventListener("input", onInput, options);
  root.addEventListener("change", onChange, options);

  paint();
  loadSessions();
  return { update, destroy };
}

function destroy() {
  mounted = false;
  abort?.abort();
  abort = null;
  rootEl = null;
  window.clearTimeout(filterTimer);
  window.clearTimeout(noteTimer);
}

function update() {}

function $(selector) {
  return rootEl?.querySelector(selector) ?? null;
}

function $$(selector) {
  return [...(rootEl?.querySelectorAll(selector) ?? [])];
}

/* ---------- 页面骨架 ---------- */

function paint() {
  if (!rootEl) return;
  rootEl.innerHTML = `
    <div class="sessions-view">
      <div class="page-head">
        <div>
          <h1 class="page-title">会话交接</h1>
          <p class="page-sub">卡住的排最前；左边挑会话，右边勾选要带走的上下文，复制给下一个 agent。扫描只在你打开或点「重新扫描」时进行。</p>
        </div>
        <div class="page-tools">
          <span class="agents" id="session-agents"></span>
          <span class="state-dot" id="session-scan-dot" data-state="warn"></span>
          <span id="session-scan-state">扫描中</span>
          <button class="btn" type="button" data-action="refresh">重新扫描</button>
        </div>
      </div>
      <div class="workspace">
        <aside class="rail" id="session-rail">
          <div class="rail-head">
            <input class="input rail-search" type="search" id="session-filter"
              placeholder="筛选标题、目录、ID 或助手" aria-label="筛选会话" autocomplete="off">
            <div class="rail-tools">
              <button class="chip" type="button" data-action="stuck-only" aria-pressed="false">只看卡住的</button>
              <span class="rail-count" id="session-rail-count"></span>
            </div>
          </div>
          <ul class="rail-list" id="session-rail-list"></ul>
          <div class="rail-note" id="session-rail-note" hidden></div>
        </aside>
        <section class="handoff" id="session-handoff" aria-live="polite">
          <div class="empty">
            <p class="empty-title">选一个会话</p>
            <p>左边列出本机编码代理留下的会话。卡住的排在最前，选中后右边就是可以直接粘贴的交接文本。</p>
          </div>
        </section>
      </div>
    </div>
  `;
}

/* ---------- 数据 ---------- */

async function loadSessions({ refresh = false } = {}) {
  if (!mounted) return;
  const dot = $("#session-scan-dot");
  const text = $("#session-scan-state");
  if (dot) dot.dataset.state = "warn";
  if (text) text.textContent = "扫描中";

  try {
    const data = await api.get(`/api/sessions${refresh ? "?refresh=1" : ""}`);
    if (!mounted) return;
    local.all = Array.isArray(data?.sessions) ? data.sessions : [];
    paintAgents(Array.isArray(data?.agents) ? data.agents : []);
    paintRail();

    if (dot) dot.dataset.state = "ok";
    if (text) text.textContent = local.all.length ? `${local.all.length} 个会话` : "没有会话";
    const note = $("#session-rail-note");
    if (note) {
      note.hidden = !data?.errors?.length;
      note.textContent = data?.errors?.length ? data.errors.join("；") : "";
    }

    // 首次进入直接打开「最该看的那条」：卡住的第一条，否则最近的一条。
    if (!local.selected && local.all.length) {
      select(local.all[0].agent, local.all[0].id);
    }
  } catch (error) {
    if (!mounted) return;
    if (dot) dot.dataset.state = "bad";
    if (text) text.textContent = "扫描失败";
    toast(error.message, "bad", "无法读取会话");
  }
}

function paintAgents(agents) {
  const host = $("#session-agents");
  if (!host) return;
  host.innerHTML = agents
    .filter((agent) => agent.present || agent.id === "opencode")
    .map(
      (agent) =>
        `<span class="agent-chip" data-absent="${agent.present ? "false" : "true"}" title="${esc(
          agent.store,
        )}">${esc(agent.label)}</span>`,
    )
    .join("");
}

function paintRail() {
  const list = $("#session-rail-list");
  const count = $("#session-rail-count");
  if (!list) return;

  const query = local.query.trim().toLowerCase();
  const rows = local.all.filter((session) => {
    if (local.stuckOnly && session.status?.state !== "stuck") return false;
    if (!query) return true;
    const label = (session.status?.label ?? "").toLowerCase();
    // 也按助手名筛选：想「只看 qoder 的」是最常见的用法之一。
    const agent = shortAgent(session.agent).toLowerCase();
    return (
      session.title.toLowerCase().includes(query) ||
      (session.cwd ?? "").toLowerCase().includes(query) ||
      session.id.toLowerCase().includes(query) ||
      agent.includes(query) ||
      label.includes(query)
    );
  });

  if (count) {
    count.textContent =
      rows.length === local.all.length ? `${rows.length} 个` : `${rows.length} / ${local.all.length}`;
  }

  if (!rows.length) {
    list.innerHTML = `<li class="rail-empty">没有符合条件的会话</li>`;
    return;
  }

  list.innerHTML = rows.map(rowHtml).join("");
  highlightSelection();
}

function rowHtml(session) {
  const stuck = session.status?.state === "stuck";
  const active = session.status?.state === "active";
  const label = session.status?.label || (active ? "进行中" : "");
  const detail = stuck || active ? trimDetail(session.status?.detail) : "";
  const tone = stuck ? "stop" : active ? "go" : "idle";
  const meta = label || detail;

  return `
    <li>
      <button class="row" type="button"
              data-agent="${esc(session.agent)}"
              data-id="${esc(session.id)}"
              data-stuck="${stuck}">
        <span class="row-top">
          <span class="row-agent">${esc(shortAgent(session.agent))}</span>
          <span class="row-ago">${esc(fmtAgo(session.updated))}</span>
        </span>
        <span class="row-title">${esc(session.title)}</span>
        ${
          meta
            ? `<span class="row-reason">
                 ${label ? `<span class="badge" data-tone="${tone}">${esc(label)}</span>` : ""}
                 ${detail ? `<span class="row-detail" title="${esc(detail)}">${esc(detail)}</span>` : ""}
               </span>`
            : ""
        }
        ${session.cwd ? `<span class="row-cwd">${esc(session.cwd)}</span>` : ""}
      </button>
    </li>`;
}

function shortAgent(agent) {
  switch (agent) {
    case "qoder-cn":
      return "qoder cn";
    case "workbuddy":
      return "workbuddy";
    default:
      return agent;
  }
}

function trimDetail(value) {
  const text = String(value ?? "")
    .replace(/\s+/g, " ")
    .trim();
  if (!text) return "";
  return text.length > 76 ? `${text.slice(0, 76)}…` : text;
}

function highlightSelection() {
  for (const row of $$(".row")) {
    const on = row.dataset.agent === local.selected?.agent && row.dataset.id === local.selected?.id;
    row.setAttribute("aria-current", on ? "true" : "false");
  }
}

/* ---------- 选中会话 ---------- */

async function select(agent, id) {
  if (local.selected?.agent === agent && local.selected?.id === id && local.detail) return;

  local.selected = { agent, id };
  local.detail = null;
  local.handoff = null;
  highlightSelection();

  const ticket = ++local.seq;
  const view = $("#session-handoff");
  if (view) view.innerHTML = `<div class="panel"><div class="panel-loading">读取会话…</div></div>`;

  try {
    const detail = await api.get(
      `/api/session?agent=${encodeURIComponent(agent)}&id=${encodeURIComponent(id)}`,
    );
    if (ticket !== local.seq || !mounted) return;
    local.detail = detail;

    // 每换一个会话就把勾选重置成该会话的实际内容：没有摘要就不勾摘要，
    // 待办和对话即使是空的也保留——「没有待办」本身就是要交接的信息。
    choice.reason = "";
    choice.note = "";
    choice.summary = true;
    choice.messages = true;
    choice.todos = true;

    paintPanel();
    buildHandoff(ticket);
  } catch (error) {
    if (ticket !== local.seq || !mounted) return;
    if (view) {
      view.innerHTML = `<div class="panel"><div class="panel-loading">读取失败：${esc(
        error.message,
      )}</div></div>`;
    }
  }
}

function paintPanel() {
  const detail = local.detail;
  const view = $("#session-handoff");
  if (!detail || !view) return;

  const status = detail.status ?? {};
  const stuck = status.state === "stuck";
  const active = status.state === "active";
  const badgeTone = stuck ? "stop" : active ? "go" : "idle";
  const badgeText = status.label || (active ? "进行中" : "已结束");

  view.innerHTML = `
    <div class="panel">
      <div class="panel-head">
        <h1 class="panel-title">${esc(detail.title)}</h1>
        <div class="panel-meta">
          <span class="badge" data-tone="${badgeTone}">${esc(badgeText)}</span>
          <span>${esc(shortAgent(detail.agent))}</span>
          <span class="mono">${esc(detail.id)}</span>
          ${detail.cwd ? `<span class="mono">${esc(detail.cwd)}</span>` : ""}
          ${detail.updated ? `<span>最后活动 ${esc(fmtAgo(detail.updated))}</span>` : ""}
        </div>
        ${alertHtml(status)}
      </div>

      <div class="controls">
        <div class="control">
          <span class="control-label">
            为什么换人
            <span class="control-hint">选「按检测结果」就用上面读到的原因</span>
          </span>
          <div class="reason-row" id="session-reason-row">
            ${REASONS.map(
              ([code, label]) =>
                `<button class="chip" type="button" data-reason="${code}" aria-pressed="${
                  choice.reason === code ? "true" : "false"
                }">${label}</button>`,
            ).join("")}
          </div>
          <span class="reason-note" id="session-reason-note"></span>
        </div>

        <div class="control">
          <label class="control-label" for="session-note">
            给接手方的一句话
            <span class="control-hint">可选：你正在做什么、下一步想怎么走</span>
          </label>
          <textarea class="textarea note-input" id="session-note" rows="2"
            placeholder="例如：接口已经跑通，还差把重试写进 worker。">${esc(choice.note)}</textarea>
        </div>

        <div class="control">
          <span class="control-label">
            带什么过去
            <span class="control-hint">每块都标了真实字数，按需勾选</span>
          </span>
          <div class="parts" id="session-parts">
            ${(detail.sections ?? []).map(partHtml).join("")}
          </div>
          <div class="extra">
            <label class="check">
              <input type="checkbox" id="session-full" ${choice.full ? "checked" : ""} />
              带上压缩前的完整对话
            </label>
            <label class="limit-field">
              对话上限
              <input class="input mono" type="number" id="session-limit" min="0" step="1000" value="${choice.limit}" />
              字
            </label>
          </div>
        </div>
      </div>

      <div class="preview-head">
        <span class="sec-title">交接文本</span>
        <span class="preview-size" id="session-preview-size"></span>
        <button class="btn primary" type="button" data-action="copy">复制</button>
        <button class="btn" type="button" data-action="save">另存为 .md</button>
      </div>
      <div class="preview-body">
        <textarea class="preview-text" id="session-preview" spellcheck="false" aria-label="交接文本"></textarea>
      </div>
      <div class="preview-foot" id="session-preview-foot"></div>

      ${foldHtml(detail)}
    </div>`;

  renderReasonNote();
}

function alertHtml(status) {
  if (status.state === "stuck") {
    const detail = status.detail ? `<span>${esc(trimDetail(status.detail))}</span>` : "";
    const at = status.at ? `<span> · ${esc(fmtDateTime(status.at))}</span>` : "";
    return `
      <div class="panel-alert" data-tone="stop">
        <p><span class="alert-strong">${esc(status.label || "会话无法继续")}</span>
        ${detail ? ` · ${detail}` : ""}${at}</p>
      </div>`;
  }
  if (status.state === "active") {
    return `
      <div class="panel-alert" data-tone="go">
        <p>这个会话看起来还在跑。如果是主动换人，现在就是干净的交接点。</p>
      </div>`;
  }
  return `
    <div class="panel-alert" data-tone="hold">
      <p>这个会话是正常结束的。把它当交接点也可以，但失败原因需要你自己写。</p>
    </div>`;
}

function partHtml(section) {
  const on =
    (section.key === "summary" && choice.summary) ||
    (section.key === "messages" && choice.messages) ||
    (section.key === "todos" && choice.todos);
  // 三段都能勾：空的段落会在交接文本里写明「没有」，这本身就是信息。
  const size = section.chars ? `${fmtInt(section.chars)} 字` : "空";
  const count = section.count ? `<span class="part-note">${fmtInt(section.count)} 条</span>` : "";
  const note = section.note ? `<span class="part-note">${esc(section.note)}</span>` : "";

  return `
    <label class="part" data-on="${on ? "true" : "false"}">
      <input type="checkbox" data-part="${esc(section.key)}" ${on ? "checked" : ""} />
      <span class="part-label">${esc(section.label)}</span>
      ${count}
      ${note}
      <span class="part-size">${size}</span>
    </label>`;
}

function foldHtml(detail) {
  const summary = detail.summary
    ? `<details class="fold">
        <summary>上次压缩摘要 · ${fmtInt(detail.summary.length)} 字</summary>
        <div class="fold-body"><div class="md">${renderMarkdown(detail.summary)}</div></div>
      </details>`
    : "";

  const pending = (detail.todos ?? []).filter(
    (todo) => todo.status !== "completed" && todo.status !== "cancelled",
  );
  const todos = pending.length
    ? `<details class="fold">
        <summary>未完成待办 · ${fmtInt(pending.length)} 条</summary>
        <div class="fold-body"><ul class="todo-list">${pending
          .map(
            (todo) =>
              `<li class="todo"><span class="todo-mark">□</span><span>${esc(todo.text)}</span></li>`,
          )
          .join("")}</ul></div>
      </details>`
    : "";

  const turns = (detail.messages ?? [])
    .map(
      (turn) => `<div class="turn" data-role="${esc(turn.role)}">
        <span class="turn-role">${esc(turn.role)}</span>
        <div class="md">${renderMarkdown(turn.text)}</div>
      </div>`,
    )
    .join("");

  const note = detail.truncated
    ? `<div class="fold-note">只显示最后 ${fmtInt(detail.messages.length)} 条；交接文本永远按完整记录生成。</div>`
    : detail.beforeCompaction
      ? `<div class="fold-note">压缩前的 ${fmtInt(detail.beforeCompaction)} 条已由摘要覆盖，这里只列摘要之后的对话。</div>`
      : "";

  const conversation = `<details class="fold">
    <summary>对话原文 · ${fmtInt((detail.messages ?? []).length)} 条</summary>
    <div class="fold-body">
      ${note}
      ${
        turns ||
        `<div class="fold-note">该会话没有可读的正文消息，可能只包含工具调用。</div>`
      }
    </div>
  </details>`;

  return summary + todos + conversation;
}

function renderReasonNote() {
  const note = $("#session-reason-note");
  if (!note) return;
  const status = local.detail?.status;
  if (!choice.reason) {
    note.textContent = status?.label
      ? `将写成：${status.label}`
      : "该会话没有记录失败原因，交接文本会按「主动换人」写。";
    return;
  }
  note.textContent = status?.label ? `检测到的是「${status.label}」` : "";
}

/* ---------- 交接文本 ---------- */

async function buildHandoff(ticket = local.seq) {
  const detail = local.detail;
  if (!detail) return;

  const preview = $("#session-preview");
  const size = $("#session-preview-size");
  const foot = $("#session-preview-foot");
  const buttons = $$('[data-action="copy"], [data-action="save"]');

  for (const button of buttons) button.disabled = true;
  if (foot) foot.textContent = "生成中…";

  try {
    const result = await api.post("/api/handoff", {
      agent: detail.agent,
      id: detail.id,
      reason: choice.reason,
      note: choice.note,
      summary: choice.summary,
      messages: choice.messages,
      todos: choice.todos,
      full: choice.full,
      limit: choice.limit,
    });
    if (ticket !== local.seq || !mounted) return;
    local.handoff = result;
    if (preview) preview.value = result.text;
    if (size) size.textContent = `${fmtInt(result.chars)} 字`;
    if (foot) foot.innerHTML = footHtml(result);
  } catch (error) {
    if (ticket !== local.seq || !mounted) return;
    if (foot) foot.innerHTML = `<span class="warn">生成失败：${esc(error.message)}</span>`;
  } finally {
    if (ticket === local.seq) {
      for (const button of buttons) button.disabled = false;
    }
  }
}

function footHtml(result) {
  const parts = [];
  if (result.clipped) {
    parts.push(
      `<span class="warn">对话已按 ${fmtInt(result.limit || 40000)} 字上限裁剪；调高上限或勾「完整对话」可补齐。</span>`,
    );
  }
  parts.push("<span>文本可直接粘贴到下一个 agent 的对话框。</span>");
  if (result.status?.state === "stuck" && result.status.detail) {
    parts.push(`<span>原始报错：${esc(trimDetail(result.status.detail))}</span>`);
  }
  return parts.join("");
}

async function copyText() {
  const preview = $("#session-preview");
  const text = preview?.value ?? "";
  if (!text.trim()) {
    toast("交接文本还是空的", "warn");
    return;
  }

  try {
    await navigator.clipboard.writeText(text);
    toast(`已复制 ${fmtInt(text.length)} 字`, "ok");
    return;
  } catch {
    // 浏览器拒绝剪贴板时退回选中，让用户按 Ctrl+C。
  }

  try {
    preview.focus();
    preview.select();
    if (document.execCommand?.("copy")) {
      toast(`已复制 ${fmtInt(text.length)} 字`, "ok");
      return;
    }
  } catch {
    // fallthrough
  }
  toast("浏览器拒绝了剪贴板访问，文本已选中，请按 Ctrl+C", "bad");
}

function saveText() {
  const preview = $("#session-preview");
  const text = preview?.value ?? "";
  if (!text.trim()) {
    toast("交接文本还是空的", "warn");
    return;
  }

  const detail = local.detail;
  const name = `${detail.agent}-${detail.id}.md`;
  const blob = new Blob([text], { type: "text/markdown;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  URL.revokeObjectURL(url);
  toast(`已保存 ${name}`, "ok");
}

/* ---------- 事件 ---------- */

function onClick(event) {
  const action = event.target.closest("[data-action]");
  if (action?.dataset.action === "refresh") {
    loadSessions({ refresh: true });
    return;
  }
  if (action?.dataset.action === "stuck-only") {
    local.stuckOnly = !local.stuckOnly;
    action.setAttribute("aria-pressed", local.stuckOnly ? "true" : "false");
    paintRail();
    return;
  }
  if (action?.dataset.action === "copy") {
    copyText();
    return;
  }
  if (action?.dataset.action === "save") {
    saveText();
    return;
  }

  const reason = event.target.closest("[data-reason]");
  if (reason) {
    choice.reason = reason.dataset.reason;
    for (const chip of $$("#session-reason-row .chip")) {
      chip.setAttribute("aria-pressed", chip === reason ? "true" : "false");
    }
    renderReasonNote();
    buildHandoff();
    return;
  }

  const row = event.target.closest(".row");
  if (row) select(row.dataset.agent, row.dataset.id);
}

function onInput(event) {
  if (event.target.id === "session-filter") {
    local.query = event.target.value;
    window.clearTimeout(filterTimer);
    filterTimer = window.setTimeout(paintRail, 200);
    return;
  }
  if (event.target.id === "session-note") {
    choice.note = event.target.value;
    window.clearTimeout(noteTimer);
    noteTimer = window.setTimeout(() => buildHandoff(), 400);
  }
}

function onChange(event) {
  const part = event.target.dataset.part;
  if (part) {
    const checked = event.target.checked;
    if (part === "summary") choice.summary = checked;
    if (part === "messages") choice.messages = checked;
    if (part === "todos") choice.todos = checked;
    const label = event.target.closest(".part");
    if (label) label.dataset.on = checked ? "true" : "false";
    buildHandoff();
    return;
  }
  if (event.target.id === "session-full") {
    choice.full = event.target.checked;
    buildHandoff();
    return;
  }
  if (event.target.id === "session-limit") {
    const value = Number(event.target.value);
    choice.limit = Number.isFinite(value) && value >= 0 ? value : 40000;
    event.target.value = choice.limit;
    buildHandoff();
  }
}
