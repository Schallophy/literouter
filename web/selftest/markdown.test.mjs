import assert from "node:assert/strict";
import test from "node:test";
import { renderMarkdown } from "../js/markdown.js";

test("renders headings, emphasis, inline code and links", () => {
  const html = renderMarkdown(
    [
      "## 结论",
      "",
      "把 `chain.models[0]` 当作起点，**不要**再写回 *active*。",
      "",
      "参考 [文档](https://example.com/a?x=1&y=2)。",
    ].join("\n"),
  );
  assert.match(html, /<h2>结论<\/h2>/);
  assert.match(html, /<code>chain\.models\[0\]<\/code>/);
  assert.match(html, /<strong>不要<\/strong>/);
  assert.match(html, /<em>active<\/em>/);
  assert.match(html, /href="https:\/\/example\.com\/a\?x=1&amp;y=2"/);
});

test("escapes transcript HTML instead of executing it", () => {
  const html = renderMarkdown('<img src=x onerror="alert(1)"> & <script>alert(2)</script>');
  assert.ok(!html.includes("<img"), html);
  assert.ok(!html.includes("<script"), html);
  assert.match(html, /&lt;img src=x onerror=&quot;alert\(1\)&quot;&gt;/);
  assert.match(html, /&lt;script&gt;alert\(2\)&lt;\/script&gt;/);
});

test("does not turn dangerous URLs into links", () => {
  const html = renderMarkdown("[点我](javascript:alert(1)) [数据](data:text/html;base64,PHN2Zz4=)");
  assert.ok(!html.includes("href=\"javascript:"), html);
  assert.ok(!html.includes("href=\"data:"), html);
  assert.ok(!html.includes("<a "), html);
});

test("keeps fenced code verbatim", () => {
  const source = ["```go", "if a < b && c > d {", "\t**not bold**", "}", "```"].join("\n");
  const html = renderMarkdown(source);
  assert.match(html, /<pre class="md-code" data-lang="go"><code>/);
  assert.match(html, /if a &lt; b &amp;&amp; c &gt; d \{/);
  assert.match(html, /\*\*not bold\*\*/);
  assert.ok(!html.includes("<strong>"), html);
});

test("renders nested lists", () => {
  const html = renderMarkdown(["- 第一项", "  - 子项 A", "  - 子项 B", "- 第二项"].join("\n"));
  assert.equal((html.match(/<ul>/g) ?? []).length, 2);
  assert.equal((html.match(/<li>/g) ?? []).length, 4);
  assert.match(html, /<li>第一项<ul><li>子项 A<\/li><li>子项 B<\/li><\/ul><\/li>/);
});

test("renders tables with alignment", () => {
  const html = renderMarkdown(["| 名称 | 数量 |", "| --- | ---: |", "| a | 1 |"].join("\n"));
  assert.match(html, /<table class="md-table">/);
  assert.match(html, /<th>名称<\/th>/);
  assert.match(html, /<th style="text-align:right">数量<\/th>/);
  assert.match(html, /<td>a<\/td><td style="text-align:right">1<\/td>/);
});

test("renders blockquotes and horizontal rules", () => {
  const html = renderMarkdown(["> 引用 **重点**", "", "---"].join("\n"));
  assert.match(html, /<blockquote><p>引用 <strong>重点<\/strong><\/p><\/blockquote>/);
  assert.match(html, /<hr>/);
});

test("treats a blank message as empty HTML", () => {
  assert.equal(renderMarkdown("   \n\n"), "");
  assert.equal(renderMarkdown(undefined), "");
});