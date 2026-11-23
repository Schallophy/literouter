package sessions

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Reason codes used by Status.Code and by the user-facing reason picker.
const (
	ReasonDetected = ""         // use whatever the store says
	ReasonQuota    = "quota"    // 额度用完
	ReasonUpstream = "upstream" // 上游报错
	ReasonAborted  = "aborted"  // 被中断
	ReasonSwitch   = "switch"   // 主动换人
	ReasonOther    = "other"    // 其他
)

// reasons maps a reason code to its heading line.
var reasons = map[string]string{
	ReasonQuota:    "额度用完，当前账号或订阅已经无法继续这个会话。",
	ReasonUpstream: "上游返回错误，当前会话无法继续。",
	ReasonAborted:  "会话被中断（进程退出、窗口关闭或手动停止），没有正常收尾。",
	ReasonSwitch:   "主动换人：原会话仍然可用，只是换一个 agent 继续。",
	ReasonOther:    "原会话无法继续。",
}

// ReasonLabel is the short chip text for a reason code.
func ReasonLabel(code string) string {
	switch code {
	case ReasonQuota:
		return "额度用完"
	case ReasonUpstream:
		return "上游报错"
	case ReasonAborted:
		return "被中断"
	case ReasonSwitch:
		return "主动换人"
	case ReasonOther:
		return "其他原因"
	}
	return ""
}

// detectedReason turns a Status into a reason code.
func detectedReason(status Status) string {
	switch status.Code {
	case "free-limit", "rate-limit", "auth":
		return ReasonQuota
	case "interrupted":
		return ReasonAborted
	case "upstream", "transport", "timeout", "invalid", "unknown":
		return ReasonUpstream
	}
	if status.Stuck() {
		return ReasonUpstream
	}
	return ReasonSwitch
}

// BuildHandoff renders the transcript for the next agent.
//
// Order matters: the next agent has to learn why it is reading this before it
// reads the conversation, otherwise it continues down the path that just
// failed.
func BuildHandoff(session Session, data transcript, options HandoffOptions) (string, []Section, bool) {
	reason := options.Reason
	if reason == ReasonDetected {
		reason = detectedReason(data.Status)
	}

	clip := options.Limit
	// A positive limit clips the messages block; zero means "everything".
	// The UI sends 40000 unless the user changes it.

	var builder strings.Builder
	builder.WriteString(header(session, data, reason, options))

	if options.Summary {
		// 「上次压缩摘要」也是固定结构的一段：没有压缩记录的会话要写明
		// 「没有压缩过」，否则接手方无法判断后面的对话是不是全部历史。
		builder.WriteString("## 上次压缩摘要\n\n")
		if summary := strings.TrimSpace(data.Summary); summary != "" {
			// 摘要自己带 `#`/`##` 标题。原样写进来会和交接文本的段落同级，
			// 让人分不清哪里是摘要、哪里是交接结构，所以整体降一级。
			builder.WriteString(demoteHeadings(summary, 1))
		} else {
			builder.WriteString("（这个会话没有压缩过，下面的对话就是全部记录。）")
		}
		builder.WriteString("\n\n")
	}

	messages := data.Messages

	clipped := false
	if options.Messages {
		// 标题与界面上的勾选项保持同一措辞：压缩之后只剩一段对话时，
		// 就叫「压缩后的对话」，不要让人以为这里的「对话」包含全部历史。
		heading := "## 对话\n\n"
		if data.BeforeCompaction > 0 && !options.Full {
			heading = "## 压缩后的对话\n\n"
		}
		builder.WriteString(heading)
		// 摘要覆盖了更早的对话；不写这一行，接手方会以为这就是全部历史。
		if data.BeforeCompaction > 0 && !options.Full {
			builder.WriteString("（更早的 " + strconv.Itoa(data.BeforeCompaction) +
				" 条已由上面的摘要覆盖。）\n\n")
		}
		if len(messages) == 0 {
			builder.WriteString("（没有可用的对话文本；该会话可能只包含工具调用或附件。）\n\n")
		}
		written, wasClipped := writeTurns(&builder, messages, clip)
		if wasClipped {
			clipped = true
			builder.WriteString(fmt.Sprintf("（上面的对话按 %d 字上限裁剪，省略了更早的 %d 条消息；如需完整上下文，调高上限或勾选「完整对话」。）\n\n",
				clip, len(messages)-written))
		}
	}

	if options.Todos {
		// 待办块始终写出：固定的交接结构里少一段，接手方会以为漏了内容。
		builder.WriteString("## 未完成待办\n\n")
		if pending := pendingTodos(data.Todos); len(pending) > 0 {
			for _, todo := range pending {
				builder.WriteString("- [ ] " + strings.TrimSpace(todo.Text) + "\n")
			}
		} else {
			builder.WriteString("（没有未完成的待办。）\n")
		}
		builder.WriteString("\n")
	}

	builder.WriteString(footer(options.Note))

	return builder.String(), buildSections(data), clipped
}

// writeTurns appends messages newest-first until the budget is spent, then
// reverses the kept window so the transcript still reads in order. A non-positive
// budget keeps everything.
func writeTurns(builder *strings.Builder, turns []Turn, budget int) (int, bool) {
	if budget <= 0 {
		for _, turn := range turns {
			builder.WriteString(renderTurn(turn))
		}
		return len(turns), false
	}

	kept := make([]Turn, 0, len(turns))
	used := 0
	for index := len(turns) - 1; index >= 0; index-- {
		size := utf8.RuneCountInString(turns[index].Text) + 16
		if len(kept) > 0 && used+size > budget {
			break
		}
		kept = append(kept, turns[index])
		used += size
	}
	for index := len(kept) - 1; index >= 0; index-- {
		builder.WriteString(renderTurn(kept[index]))
	}
	return len(kept), len(kept) < len(turns)
}

// demoteHeadings shifts every Markdown heading in a block down by `levels`, so a
// summary or a transcript keeps its own outline as a sub-outline of the handoff
// section carrying it.
//
// A fixed shift (rather than "only as deep as needed") is deliberate: transcripts
// are mixed documents, and a rule that depends on the shallowest heading in one
// message would give neighbouring messages different depths. Levels cap at six,
// where extra depth stops conveying anything anyway.
//
// Fenced code is skipped: a `# comment` inside a snippet is not a heading.
func demoteHeadings(text string, levels int) string {
	if text == "" || levels <= 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	fence := ""
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if marker, ok := fenceMarker(trimmed); ok {
			switch {
			case fence == "":
				fence = marker
			case strings.HasPrefix(trimmed, fence) && len(trimmed) >= len(fence):
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		level, body, ok := splitHeading(trimmed)
		if !ok {
			continue
		}
		level += levels
		if level > 6 {
			level = 6
		}
		lines[index] = strings.Repeat("#", level) + " " + body
	}
	return strings.Join(lines, "\n")
}

// fenceMarker reports the opening run of a code fence. Markdown needs at least
// three backticks or tildes, so a line starting with one or two is ordinary text.
func fenceMarker(line string) (string, bool) {
	if len(line) < 3 || line[0] != '`' && line[0] != '~' {
		return "", false
	}
	run := 0
	for run < len(line) && line[run] == line[0] {
		run++
	}
	if run < 3 {
		return "", false
	}
	if run > 4 {
		run = 4
	}
	return strings.Repeat(string(line[0]), run), true
}

// splitHeading recognises `# text` up to `###### text`.
func splitHeading(line string) (int, string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := strings.TrimSpace(line[level:])
	if rest == "" {
		return 0, "", false
	}
	return level, rest, true
}

func renderTurn(turn Turn) string {
	role := turn.Role
	if role == "" {
		role = "助手"
	}
	// 对话正文的标题降两级：原文常用 `#`/`##` 做自己的提纲，不降就会和
	// 交接文本的段落同级。代码块被跳过，里面的 `# 注释` 不会被当成标题。
	return "**" + role + "**：" + demoteHeadings(strings.TrimSpace(turn.Text), 2) + "\n\n"
}

func header(session Session, data transcript, reason string, options HandoffOptions) string {
	var builder strings.Builder
	builder.WriteString("# 会话交接：" + nonEmpty(session.Title, session.ID) + "\n\n")

	heading := reasons[reason]
	if heading == "" {
		heading = reasons[ReasonOther]
	}
	builder.WriteString("## 为什么换人\n\n" + heading + "\n")

	// 原会话自己记录的错误，无论如何都写出来：接手方需要知道上一棒到底
	// 是怎么掉的，而不是只读到一句概括。
	if data.Status.Stuck() && data.Status.Detail != "" {
		builder.WriteString("\n原会话记录的失败：" + describeStatus(data.Status) + "\n")
	}
	if options.Reason != ReasonDetected && options.Reason != "" && data.Status.Stuck() {
		builder.WriteString("\n（你手动选择了上面的原因，覆盖了原会话的记录。）\n")
	}
	builder.WriteString("\n")

	builder.WriteString("## 来源\n\n")
	builder.WriteString("- 助手：" + agentLabel(session.Agent) + "（" + session.ID + "）\n")
	if session.Cwd != "" {
		builder.WriteString("- 工作目录：" + session.Cwd + "\n")
	}
	if session.Updated != "" {
		builder.WriteString("- 最后活动：" + session.Updated + "\n")
	}
	if data.SummaryAt != "" {
		builder.WriteString("- 上次压缩：" + data.SummaryAt + "\n")
	}
	if note := strings.TrimSpace(options.Note); note != "" {
		builder.WriteString("- 交接说明：" + oneLine(note) + "\n")
	}
	builder.WriteString("\n")
	return builder.String()
}

func footer(note string) string {
	var builder strings.Builder
	builder.WriteString("## 请接手\n\n")
	if text := strings.TrimSpace(note); text != "" {
		builder.WriteString(text + "\n\n")
	}
	builder.WriteString("以上是上一个会话的上下文。请在此基础上继续，不要重复已完成的工作；")
	builder.WriteString("如果信息不足，先说明你需要的文件或确认点。\n")
	return builder.String()
}

// describeStatus renders a status as one short line for the header.
func describeStatus(status Status) string {
	parts := make([]string, 0, 2)
	if status.Label != "" {
		parts = append(parts, status.Label)
	}
	if status.Detail != "" {
		parts = append(parts, status.Detail)
	}
	if len(parts) == 0 {
		return status.State
	}
	return strings.Join(parts, "：")
}

func oneLine(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.Join(strings.Fields(value), " ")
}

// cleanText normalises a message body: line endings, injected reminders and
// wrapper tags that the user never typed.
func cleanText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	if strings.Contains(value, "<system-reminder") {
		value = systemReminderPattern.ReplaceAllString(value, "")
	}
	if strings.Contains(value, "<user_info") {
		value = userInfoPattern.ReplaceAllString(value, "")
	}
	value = wrapperTagPattern.ReplaceAllString(value, "")
	value = strings.TrimSpace(value)
	for strings.Contains(value, "\n\n\n") {
		value = strings.ReplaceAll(value, "\n\n\n", "\n\n")
	}
	return value
}

func cleanSummary(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSpace(value)
}

func appendTurn(list []Turn, role, text string) []Turn {
	text = cleanText(text)
	if text == "" {
		return list
	}
	return append(list, Turn{Role: role, Text: text})
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return "…"
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

// titleFromText derives a one-line title from the user's first message.
//
// Agents often inject a fixed boilerplate header ("A goal has been set…") before
// the user's own words, and goals usually start with a Markdown heading. Skipping
// that scaffolding is what turns a list row into something the user recognises.
func titleFromText(value string, limit int) string {
	value = cleanText(value)
	if strings.TrimSpace(value) == "" {
		return ""
	}

	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		// 固定套话与包装标签都不是用户写的话。
		if strings.HasPrefix(lower, "a goal has been set") ||
			strings.HasPrefix(lower, "the objective below") ||
			strings.HasPrefix(lower, "this session is being continued") ||
			strings.HasPrefix(lower, "<") {
			continue
		}
		line = strings.TrimLeft(line, "#>*-•· \t")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return truncateRunes(strings.Join(strings.Fields(line), " "), limit)
	}
	return truncateRunes(strings.Join(strings.Fields(value), " "), limit)
}

func formatMillis(value int64) string {
	if value <= 0 {
		return ""
	}
	return time.UnixMilli(value).Format(time.RFC3339)
}
