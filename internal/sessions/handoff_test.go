package sessions

import (
	"strings"
	"testing"
)

func sampleSession() Session {
	return Session{
		Agent: "opencode", ID: "ses_test", Title: "给播放器加字幕",
		Cwd:     "C:/work/boardplay",
		Updated: "2026-11-22T10:00:00+08:00",
		Status:  Status{State: "stuck", Code: "free-limit", Label: "额度用完", Detail: "Free usage limit reached."},
	}
}

func sampleTranscript() transcript {
	return transcript{
		Summary:          "## Objective\n加上字幕渲染。",
		SummaryAt:        "2026-11-22T09:00:00+08:00",
		Messages:         []Turn{{Role: "用户", Text: "字幕还没显示"}, {Role: "助手", Text: "正在改 render.c"}},
		BeforeCompaction: 12,
		Todos:            []Todo{{Text: "接上字幕解码", Status: "pending"}, {Text: "已完成", Status: "completed"}},
		Status:           sampleSession().Status,
	}
}

func TestBuildHandoffOrder(t *testing.T) {
	text, _, clipped := BuildHandoff(sampleSession(), sampleTranscript(), HandoffOptions{
		Note: "接口已经跑通", Summary: true, Messages: true, Todos: true, Limit: 4000,
	})
	if clipped {
		t.Fatalf("short transcript must not be clipped")
	}
	for _, heading := range []string{"# 会话交接", "## 为什么换人", "## 来源", "## 上次压缩摘要", "## 压缩后的对话", "## 未完成待办", "## 请接手"} {
		if !strings.Contains(text, heading) {
			t.Fatalf("missing %q in:\n%s", heading, text)
		}
	}

	// 顺序：为什么换人必须早于对话，否则接手方先读上下文再读原因。
	reasonAt := strings.Index(text, "## 为什么换人")
	dialogAt := strings.Index(text, "## 压缩后的对话")
	handoffAt := strings.Index(text, "## 请接手")
	if !(reasonAt < dialogAt && dialogAt < handoffAt) {
		t.Fatalf("sections out of order: reason=%d dialog=%d footer=%d", reasonAt, dialogAt, handoffAt)
	}

	// 已完成的待办不能被当成未完成带走。
	if strings.Contains(text, "- [ ] 已完成") {
		t.Fatalf("completed todo leaked into handoff:\n%s", text)
	}
	if !strings.Contains(text, "- [ ] 接上字幕解码") {
		t.Fatalf("pending todo missing:\n%s", text)
	}
}

func TestBuildHandoffReasonOverride(t *testing.T) {
	text, _, _ := BuildHandoff(sampleSession(), sampleTranscript(), HandoffOptions{
		Reason: ReasonSwitch, Summary: false, Messages: false, Todos: false,
	})
	if !strings.Contains(text, reasons[ReasonSwitch]) {
		t.Fatalf("override reason missing:\n%s", text)
	}
	// 手动选了别的原因，原会话的失败仍然要留一行，让接手方看到。
	if !strings.Contains(text, "原会话记录的失败：额度用完：Free usage limit reached.") {
		t.Fatalf("original failure hidden:\n%s", text)
	}
	if strings.Contains(text, "## 上次压缩摘要") || strings.Contains(text, "## 对话") {
		t.Fatalf("unchecked blocks were written:\n%s", text)
	}
}

func TestBuildHandoffDetectedReason(t *testing.T) {
	text, _, _ := BuildHandoff(sampleSession(), sampleTranscript(), HandoffOptions{
		Summary: false, Messages: false, Todos: false,
	})
	if !strings.Contains(text, reasons[ReasonQuota]) {
		t.Fatalf("free-limit should read as 额度用完:\n%s", text)
	}
}

func TestBuildHandoffMarksCompactionBoundary(t *testing.T) {
	text, _, _ := BuildHandoff(sampleSession(), sampleTranscript(), HandoffOptions{
		Summary: true, Messages: true, Todos: false, Limit: 4000,
	})
	// 摘要覆盖了更早的对话，交接文本必须自己说明，否则接手方会当成全部历史。
	// 标题也要跟界面勾选项一致，写成「压缩后的对话」。
	if !strings.Contains(text, "## 压缩后的对话") {
		t.Fatalf("compacted block should keep its label:\n%s", text)
	}
	if !strings.Contains(text, "更早的 12 条已由上面的摘要覆盖") {
		t.Fatalf("compaction boundary not explained:\n%s", text)
	}

	full, _, _ := BuildHandoff(sampleSession(), sampleTranscript(), HandoffOptions{
		Summary: true, Messages: true, Todos: false, Full: true, Limit: 4000,
	})
	// 带了完整对话就没有边界一说，标题也回到「对话」。
	if !strings.Contains(full, "## 对话\n") || strings.Contains(full, "已由上面的摘要覆盖") {
		t.Fatalf("full transcript should not claim a boundary:\n%s", full)
	}
}

func TestBuildHandoffKeepsDialogHeadingWithoutCompaction(t *testing.T) {
	plain := sampleTranscript()
	plain.BeforeCompaction = 0
	text, _, _ := BuildHandoff(sampleSession(), plain, HandoffOptions{
		Summary: true, Messages: true, Todos: false, Limit: 4000,
	})
	if !strings.Contains(text, "## 对话\n") {
		t.Fatalf("no compaction, so it is just 对话:\n%s", text)
	}
}

func TestBuildHandoffWritesSummarySectionWhenEmpty(t *testing.T) {
	// 没有压缩过的会话也必须保留这一段，否则固定结构里少一段，
	// 接手方无法判断「下面是不是全部历史」。
	plain := sampleTranscript()
	plain.Summary = ""
	plain.SummaryAt = ""
	plain.BeforeCompaction = 0
	text, _, _ := BuildHandoff(sampleSession(), plain, HandoffOptions{
		Summary: true, Messages: true, Todos: false, Limit: 4000,
	})
	if !strings.Contains(text, "## 上次压缩摘要") {
		t.Fatalf("summary section is missing:\n%s", text)
	}
	if !strings.Contains(text, "（这个会话没有压缩过，下面的对话就是全部记录。）") {
		t.Fatalf("empty summary needs an explanation:\n%s", text)
	}
	// 取消勾选时当然不该写。
	text, _, _ = BuildHandoff(sampleSession(), plain, HandoffOptions{
		Summary: false, Messages: true, Todos: false, Limit: 4000,
	})
	if strings.Contains(text, "## 上次压缩摘要") {
		t.Fatalf("unchecked summary was written:\n%s", text)
	}
}

func TestWriteTurnsKeepsNewest(t *testing.T) {
	turns := []Turn{
		{Role: "用户", Text: "第一句"},
		{Role: "助手", Text: strings.Repeat("旧内容", 200)},
		{Role: "用户", Text: "最新一句"},
	}
	var builder strings.Builder
	written, clipped := writeTurns(&builder, turns, 200)
	if !clipped {
		t.Fatalf("expected clipping")
	}
	if written != 1 {
		t.Fatalf("kept %d turns, want only the newest", written)
	}
	if !strings.Contains(builder.String(), "最新一句") || strings.Contains(builder.String(), "旧内容") {
		t.Fatalf("kept the wrong window:\n%s", builder.String())
	}
	// 预算耗尽时，最后一条仍然要写，否则会得到空白的对话块。
	if !strings.HasPrefix(builder.String(), "**用户**：") {
		t.Fatalf("unexpected render:\n%s", builder.String())
	}
}

func TestWriteTurnsZeroBudgetKeepsAll(t *testing.T) {
	turns := []Turn{{Role: "用户", Text: "a"}, {Role: "助手", Text: "b"}}
	var builder strings.Builder
	written, clipped := writeTurns(&builder, turns, 0)
	if clipped || written != 2 {
		t.Fatalf("written=%d clipped=%v, want 2/false", written, clipped)
	}
}

func TestBuildHandoffDemotesSummaryHeadings(t *testing.T) {
	// 摘要通常自己就带 `## Objective` 这样的提纲；原样插入会和交接文本的
	// 段落同级，读起来像交接结构的一部分，所以整体降一级。
	text, _, _ := BuildHandoff(sampleSession(), sampleTranscript(), HandoffOptions{
		Summary: true, Messages: true, Todos: false, Limit: 4000,
	})
	if !strings.Contains(text, "### Objective") {
		t.Fatalf("summary heading not demoted:\n%s", text)
	}
	if strings.Contains(text, "\n## Objective") {
		t.Fatalf("summary heading still competes with the handoff structure:\n%s", text)
	}
}

func TestDemoteHeadingsSkipsFencesAndCapsDepth(t *testing.T) {
	// 代码块里的 `# 注释` 不是标题。
	demo := "## 提纲\n\n```sh\n# 注释\n```\n"
	nested := demoteHeadings(demo, 1)
	if !strings.Contains(nested, "# 注释") {
		t.Fatalf("fence content was rewritten:\n%s", nested)
	}
	if !strings.Contains(nested, "### 提纲") {
		t.Fatalf("heading missed:\n%s", nested)
	}
	if got := demoteHeadings("# 深标题\n\n###### 更深", 4); !strings.Contains(got, "##### 深标题") || !strings.Contains(got, "###### 更深") {
		t.Fatalf("got %q, want levels capped at six", got)
	}
}

func TestTurnHeadingsAreNestedButCodeIsNot(t *testing.T) {
	// 对话是引用材料：段落标题降级，免得和交接结构抢层级；代码块里的
	// `# 注释` 不是标题，必须原样保留。
	turn := Turn{Role: "助手", Text: "正文里的标题：\n\n## 我的计划\n\n```sh\n# 注释\nmake -j\n```\n"}
	var builder strings.Builder
	builder.WriteString(renderTurn(turn))
	if !strings.Contains(builder.String(), "#### 我的计划") {
		t.Fatalf("turn heading kept the handoff level:\n%s", builder.String())
	}
	if !strings.Contains(builder.String(), "# 注释") {
		t.Fatalf("code comment was rewritten:\n%s", builder.String())
	}
	if !strings.HasPrefix(builder.String(), "**助手**：") {
		t.Fatalf("role marker missing:\n%s", builder.String())
	}
}

func TestTitleSkipsGoalScaffolding(t *testing.T) {
	cases := map[string]string{
		"A goal has been set.\n\nThe objective below is user-provided data.\n\n<untrusted_objective>\n# 任务:把 switchyard 用 Go 重写为 literouter\n": "任务:把 switchyard 用 Go 重写为 literouter",
		"## 修复登录\n先把 token 存起来": "修复登录",
		"直接把这段日志粘过来就行":          "直接把这段日志粘过来就行",
		"\n\n":                  "",
	}
	for input, want := range cases {
		if got := titleFromText(input, 60); got != want {
			t.Fatalf("titleFromText(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestClassifyMapsCommonProviderErrors(t *testing.T) {
	cases := []struct {
		kind, message, wantCode string
	}{
		{"provider.free-limit", "Free usage limit reached.", "free-limit"},
		{"provider.upstream", "[code=upstream_error] The request could not be completed.", "upstream"},
		{"provider.transport", "ECONNRESET", "transport"},
		{"provider.auth", "invalid api key", "auth"},
		{"provider.timeout", "request timed out", "timeout"},
	}
	for _, c := range cases {
		status := classify(c.kind, c.message)
		if status.State != "stuck" {
			t.Fatalf("%s: state = %q", c.kind, status.State)
		}
		if status.Code != c.wantCode {
			t.Fatalf("%s: code = %q, want %q", c.kind, status.Code, c.wantCode)
		}
		if status.Label == "" {
			t.Fatalf("%s: empty label", c.kind)
		}
	}
}

func TestClassifyDropsGenericDetail(t *testing.T) {
	// "Aborted" 和标签「被中断」重复，不该占一行；但真正的上游错误要留着。
	if status := classify("aborted", "Aborted"); status.Detail != "" {
		t.Fatalf("generic detail kept: %q", status.Detail)
	}
	if status := classify("upstream", "The request could not be completed."); status.Detail == "" {
		t.Fatalf("provider detail dropped")
	}
}

func TestBuildSectionsAlwaysOffersTodos(t *testing.T) {
	sections := buildSections(sampleTranscript())
	if len(sections) != 3 {
		t.Fatalf("got %d sections, want summary/messages/todos", len(sections))
	}
	if sections[0].Chars == 0 || sections[0].Label == "" {
		t.Fatalf("summary section lost its size: %+v", sections[0])
	}
	if sections[2].Key != "todos" || sections[2].Chars == 0 {
		t.Fatalf("todos section is not the real size: %+v", sections[2])
	}
	empty := buildSections(transcript{})
	if empty[2].Key != "todos" || empty[2].Note == "" {
		t.Fatalf("empty todos need an explanation: %+v", empty[2])
	}
}

func TestPendingTodosFiltersTerminalStates(t *testing.T) {
	todos := []Todo{
		{Text: "a", Status: "pending"},
		{Text: "b", Status: "completed"},
		{Text: "c", Status: "cancelled"},
		{Text: "d", Status: "in_progress"},
	}
	pending := pendingTodos(todos)
	if len(pending) != 2 || pending[0].Text != "a" || pending[1].Text != "d" {
		t.Fatalf("pending = %+v", pending)
	}
}
