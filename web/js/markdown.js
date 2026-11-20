// A small Markdown renderer for agent transcripts.
//
// The dashboard has no build step, so this stays dependency-free. Every piece
// of source text is escaped before any tag is added: transcripts are data, and
// pasted logs can contain HTML that must never run inside the dashboard.

const ESCAPES = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
};

const PLACEHOLDER = /\u0000(\d+)\u0000/g;

const FENCE = /^(\s*)(`{3,}|~{3,})\s*([\w+#.-]*)\s*$/;
const HEADING = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/;
const RULE = /^\s{0,3}([-*_])(?:\s*\1){2,}\s*$/;
const QUOTE = /^\s{0,3}>\s?/;
const LIST_ITEM = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;

function esc(value) {
  return String(value ?? "").replace(/[&<>"']/g, (char) => ESCAPES[char]);
}

/** Renders Markdown source to HTML that is safe to insert into the page. */
export function renderMarkdown(source) {
  const text = String(source ?? "").replace(/\r\n?/g, "\n");
  if (!text.trim()) return "";
  return renderBlocks(text.split("\n"));
}

function renderBlocks(lines) {
  const out = [];
  let index = 0;

  while (index < lines.length) {
    const line = lines[index];
    if (!line.trim()) {
      index += 1;
      continue;
    }

    const fence = FENCE.exec(line);
    if (fence) {
      const indent = indentWidth(fence[1]);
      const marker = fence[2][0];
      const language = fence[3];
      const body = [];
      index += 1;
      while (index < lines.length) {
        if (isFenceClose(lines[index], marker)) {
          index += 1;
          break;
        }
        body.push(stripIndent(lines[index], indent));
        index += 1;
      }
      out.push(codeBlock(body.join("\n"), language));
      continue;
    }

    const heading = HEADING.exec(line);
    if (heading) {
      const level = heading[1].length;
      out.push(`<h${level}>${inline(heading[2])}</h${level}>`);
      index += 1;
      continue;
    }

    if (RULE.test(line)) {
      out.push("<hr>");
      index += 1;
      continue;
    }

    if (QUOTE.test(line)) {
      const quoted = [];
      while (index < lines.length && QUOTE.test(lines[index])) {
        quoted.push(lines[index].replace(QUOTE, ""));
        index += 1;
      }
      out.push(`<blockquote>${renderBlocks(quoted)}</blockquote>`);
      continue;
    }

    if (isTableStart(lines, index)) {
      const table = renderTable(lines, index);
      out.push(table.html);
      index = table.next;
      continue;
    }

    if (LIST_ITEM.test(line)) {
      const list = renderList(lines, index);
      out.push(list.html);
      index = Math.max(list.next, index + 1);
      continue;
    }

    const paragraph = [line];
    index += 1;
    while (
      index < lines.length &&
      lines[index].trim() &&
      !startsBlock(lines[index]) &&
      !isTableStart(lines, index)
    ) {
      paragraph.push(lines[index]);
      index += 1;
    }
    out.push(`<p>${inline(paragraph.join("\n"))}</p>`);
  }

  return out.join("");
}

function startsBlock(line) {
  return FENCE.test(line) || HEADING.test(line) || RULE.test(line) || QUOTE.test(line) || LIST_ITEM.test(line);
}

function codeBlock(body, language) {
  const lang = language ? ` data-lang="${esc(language)}"` : "";
  return `<pre class="md-code"${lang}><code>${esc(body)}</code></pre>`;
}

function isFenceClose(line, marker) {
  const trimmed = line.trim();
  if (!trimmed) return false;
  let count = 0;
  while (count < trimmed.length && trimmed[count] === marker) count += 1;
  return count >= 3 && trimmed.slice(count).trim() === "";
}

// ---- tables ---------------------------------------------------------------

function isTableStart(lines, index) {
  if (index + 1 >= lines.length) return false;
  if (!lines[index].includes("|")) return false;
  return tableAligns(lines[index + 1]) !== null;
}

function tableAligns(line) {
  if (!line.includes("|")) return null;
  const cells = splitRow(line);
  if (!cells.length) return null;
  const aligns = [];
  for (const cell of cells) {
    const value = cell.trim();
    if (!/^:?-{2,}:?$/.test(value)) return null;
    const left = value.startsWith(":");
    const right = value.endsWith(":");
    aligns.push(left && right ? "center" : right ? "right" : left ? "left" : "");
  }
  return aligns;
}

function splitRow(line) {
  let value = line.trim();
  if (value.startsWith("|")) value = value.slice(1);
  if (value.endsWith("|") && !value.endsWith("\\|")) value = value.slice(0, -1);
  const cells = [];
  let current = "";
  for (let index = 0; index < value.length; index += 1) {
    const char = value[index];
    if (char === "\\" && value[index + 1] === "|") {
      current += "|";
      index += 1;
      continue;
    }
    if (char === "|") {
      cells.push(current);
      current = "";
      continue;
    }
    current += char;
  }
  cells.push(current);
  return cells;
}

function renderTable(lines, start) {
  const aligns = tableAligns(lines[start + 1]) ?? [];
  const cell = (tag, content, column) => {
    const align = aligns[column];
    const style = align ? ` style="text-align:${align}"` : "";
    return `<${tag}${style}>${inline(content.trim())}</${tag}>`;
  };

  const headers = splitRow(lines[start]);
  const head = `<thead><tr>${headers
    .map((content, column) => cell("th", content, column))
    .join("")}</tr></thead>`;

  const rows = [];
  let index = start + 2;
  while (index < lines.length && lines[index].trim() && lines[index].includes("|")) {
    if (startsBlock(lines[index])) break;
    const cells = splitRow(lines[index]);
    rows.push(`<tr>${cells.map((content, column) => cell("td", content, column)).join("")}</tr>`);
    index += 1;
  }

  return {
    html: `<div class="md-table-wrap"><table class="md-table">${head}<tbody>${rows.join(
      "",
    )}</tbody></table></div>`,
    next: index,
  };
}

// ---- lists ----------------------------------------------------------------

function listMatch(line) {
  const match = LIST_ITEM.exec(line);
  if (!match) return null;
  return {
    indent: indentWidth(match[1]),
    ordered: /^\d/.test(match[2]),
    text: match[3],
  };
}

function renderList(lines, start) {
  const first = listMatch(lines[start]);
  const baseIndent = first.indent;
  const ordered = first.ordered;
  const items = [];
  let index = start;

  while (index < lines.length) {
    const line = lines[index];
    const match = listMatch(line);

    if (match) {
      if (match.indent < baseIndent) break;
      if (match.indent === baseIndent && match.ordered !== ordered) break;
      if (match.indent > baseIndent) {
        if (!items.length) break;
        const nested = renderList(lines, index);
        items[items.length - 1].nested.push(nested.html);
        index = Math.max(nested.next, index + 1);
        continue;
      }
      items.push({ lines: [match.text], nested: [] });
      index += 1;
      continue;
    }

    if (!line.trim()) {
      let look = index + 1;
      while (look < lines.length && !lines[look].trim()) look += 1;
      if (look >= lines.length) {
        index = look;
        break;
      }
      const next = listMatch(lines[look]);
      const indented = /^\s{2,}\S/.test(lines[look]);
      if ((next && next.indent >= baseIndent) || indented) {
        index = look;
        continue;
      }
      break;
    }

    if (!items.length || !/^\s{2,}\S/.test(line)) break;
    items[items.length - 1].lines.push(line);
    index += 1;
  }

  const tag = ordered ? "ol" : "ul";
  const body = items
    .map((item) => {
      const itemLines = dedent(item.lines);
      const content = itemLines.length === 1 ? inline(itemLines[0]) : renderBlocks(itemLines);
      return `<li>${content}${item.nested.join("")}</li>`;
    })
    .join("");

  return { html: `<${tag}>${body}</${tag}>`, next: index };
}

function dedent(lines) {
  if (lines.length <= 1) return lines;
  let min = Infinity;
  for (const line of lines.slice(1)) {
    if (!line.trim()) continue;
    const width = indentWidth(line.match(/^\s*/)[0]);
    if (width < min) min = width;
  }
  if (!Number.isFinite(min)) min = 0;
  return [lines[0], ...lines.slice(1).map((line) => stripIndent(line, min))];
}

function indentWidth(value) {
  let width = 0;
  for (const char of value) width += char === "\t" ? 4 : 1;
  return width;
}

function stripIndent(line, width) {
  let index = 0;
  let removed = 0;
  while (index < line.length && removed < width) {
    const char = line[index];
    if (char === " ") {
      removed += 1;
      index += 1;
    } else if (char === "\t") {
      removed += 4;
      index += 1;
    } else {
      break;
    }
  }
  return line.slice(index);
}

// ---- inline ---------------------------------------------------------------

function inline(value) {
  const tokens = [];
  const keep = (html) => `\u0000${tokens.push(html) - 1}\u0000`;

  let text = esc(value).replace(/\n/g, "<br>");

  text = text.replace(/`([^`]+)`/g, (_, code) => keep(`<code>${code}</code>`));

  text = text.replace(/!\[([^\]]*)\]\(\s*([^)\s]+)\s*\)/g, (match, alt, url) => {
    const href = safeUrl(url);
    if (!href) return match;
    const label = alt ? `[图片] ${inline(alt)}` : "[图片]";
    return keep(
      `<a class="md-image" href="${href}" target="_blank" rel="noreferrer noopener">${label}</a>`,
    );
  });

  text = text.replace(/\[([^\]]+)\]\(\s*([^)\s]+)\s*\)/g, (match, label, url) => {
    const href = safeUrl(url);
    if (!href) return match;
    return keep(`<a href="${href}" target="_blank" rel="noreferrer noopener">${inline(label)}</a>`);
  });

  text = text.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  text = text.replace(/__([^_]+)__/g, "<strong>$1</strong>");
  text = text.replace(/~~([^~]+)~~/g, "<del>$1</del>");
  text = text.replace(/(^|[^*])\*([^*\n]+)\*/g, "$1<em>$2</em>");
  text = text.replace(/(^|[^\w])_([^_\n]+)_(?=[^\w]|$)/g, "$1<em>$2</em>");

  return text.replace(PLACEHOLDER, (_, index) => tokens[Number(index)] ?? "");
}

// Only http(s) and mailto links are rendered as links; relative targets keep
// working. Anything else (javascript:, data:, vbscript:, ...) stays as text.
function safeUrl(value) {
  const url = value.trim();
  if (/^https?:/i.test(url)) return url;
  if (/^mailto:/i.test(url)) return url;
  if (/^[a-z][a-z0-9+.-]*:/i.test(url)) return "";
  return url;
}