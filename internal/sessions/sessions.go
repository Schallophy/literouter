// Package sessions discovers the local session stores of coding agents
// (OpenCode, Qoder, Qoder CN, WorkBuddy, ZCode) and builds handoff transcripts
// from the last compaction summary plus the messages that followed it.
package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const cacheTTL = 8 * time.Second

// Session is one conversation found in an agent's local store.
type Session struct {
	Agent   string `json:"agent"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Cwd     string `json:"cwd"`
	Updated string `json:"updated"`
	Created string `json:"created,omitempty"`
	Source  string `json:"source"`

	kind string
	file string
}

// Agent reports whether an agent is installed on this machine.
type Agent struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Present bool   `json:"present"`
	Store   string `json:"store"`
}

// ListResult is the payload of GET /api/sessions.
type ListResult struct {
	Sessions []Session `json:"sessions"`
	Agents   []Agent   `json:"agents"`
	Errors   []string  `json:"errors,omitempty"`
}

// HandoffResult is the payload of GET /api/sessions/handoff.
type HandoffResult struct {
	Agent string `json:"agent"`
	ID    string `json:"id"`
	Title string `json:"title"`
	Text  string `json:"text"`
	Chars int    `json:"chars"`
}

// DetailResult is the payload of GET /api/sessions/detail.
type DetailResult struct {
	Agent     string          `json:"agent"`
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Cwd       string          `json:"cwd,omitempty"`
	Updated   string          `json:"updated,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	SummaryAt string          `json:"summaryAt,omitempty"`
	Messages  []DetailMessage `json:"messages"`
	Chars     int             `json:"chars"`
}

// DetailMessage is one readable turn in a session viewer.
type DetailMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type message struct {
	Role string
	Text string
}

// transcript is the readable context of one session: the last compaction
// summary an agent stored (when available) and the messages after it.
type transcript struct {
	Summary   string
	SummaryAt string
	Messages  []message
}

// Detector scans the local agent stores. It is safe for concurrent use.
type Detector struct {
	home        string
	opencodeCLI string
	roots       []string

	mu       sync.Mutex
	cache    ListResult
	cachedAt time.Time
}

// NewDetector resolves the local home, the OpenCode CLI and the project roots.
func NewDetector() *Detector {
	home, _ := os.UserHomeDir()
	return &Detector{
		home:        home,
		opencodeCLI: findOpencodeCLI(home),
		roots:       opencodeRoots(home),
	}
}

// List returns every session found in the local agent stores, newest first.
func (d *Detector) List() ListResult {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.cachedAt.IsZero() && time.Since(d.cachedAt) < cacheTTL {
		return d.cache
	}

	result := ListResult{Agents: d.detectAgents()}
	var all []Session
	merge := func(items []Session, err error, label string) {
		all = append(all, items...)
		if err != nil && len(items) == 0 {
			result.Errors = append(result.Errors, label+"："+err.Error())
		}
	}

	items, err := d.opencodeSessions()
	merge(items, err, "OpenCode")
	items, err = d.qoderSessions("qoder", joinHome(d.home, ".qoder", "projects"))
	merge(items, err, "Qoder")
	items, err = d.qoderSessions("qoder-cn", joinHome(d.home, ".qoder-cn", "projects"))
	merge(items, err, "Qoder CN")
	items, err = d.workbuddySessions(joinHome(d.home, ".workbuddy", "projects"))
	merge(items, err, "WorkBuddy")
	items, err = d.zcodeSessions()
	merge(items, err, "ZCode")

	for index := range all {
		all[index].Source = d.prettyPath(all[index].Source)
	}

	sortSessions(all)
	result.Sessions = all
	d.cache = result
	d.cachedAt = time.Now()
	return result
}

// Refresh drops the cache and scans again.
func (d *Detector) Refresh() ListResult {
	d.mu.Lock()
	d.cachedAt = time.Time{}
	d.mu.Unlock()
	return d.List()
}

// Handoff builds the handoff transcript for one session: the last compaction
// summary plus every message the agent produced after it.
func (d *Detector) Handoff(agent, id string) (HandoffResult, error) {
	session, err := d.find(agent, id)
	if err != nil {
		return HandoffResult{}, err
	}

	data, err := d.readTranscript(*session, false)
	if err != nil {
		return HandoffResult{}, err
	}

	text := BuildHandoffTranscript(*session, data)
	return HandoffResult{
		Agent: session.Agent, ID: session.ID, Title: session.Title,
		Text: text, Chars: utf8.RuneCountInString(text),
	}, nil
}

// Detail returns the readable transcript for the viewer. Unlike Handoff it
// keeps the conversation that came before the last compaction, so opening a
// session shows the whole history instead of only the handoff context.
func (d *Detector) Detail(agent, id string) (DetailResult, error) {
	session, err := d.find(agent, id)
	if err != nil {
		return DetailResult{}, err
	}

	data, err := d.readTranscript(*session, true)
	if err != nil {
		return DetailResult{}, err
	}

	result := DetailResult{
		Agent: session.Agent, ID: session.ID, Title: session.Title,
		Cwd: session.Cwd, Updated: session.Updated,
		Summary: data.Summary, SummaryAt: data.SummaryAt,
		Messages: make([]DetailMessage, 0, len(data.Messages)),
	}
	result.Chars = utf8.RuneCountInString(result.Summary)
	for _, item := range data.Messages {
		result.Messages = append(result.Messages, DetailMessage{Role: item.Role, Text: item.Text})
		result.Chars += utf8.RuneCountInString(item.Text)
	}
	return result, nil
}

func (d *Detector) readTranscript(session Session, full bool) (transcript, error) {
	var data transcript
	var err error
	switch session.kind {
	case "opencode":
		data, err = d.opencodeTranscript(session, full)
	case "qoder":
		data, err = qoderTranscript(session.file, full)
	case "workbuddy":
		data, err = workbuddyTranscript(session.file, full)
	case "zcode":
		data.Messages, err = zcodeTranscript(session.file, full)
	case "zcode-db":
		data, err = zcodeDBTranscript(session.file, session.ID, full)
	default:
		err = errors.New("未知的会话来源")
	}
	return data, err
}

func (d *Detector) find(agent, id string) (*Session, error) {
	if session := lookupSession(d.List().Sessions, agent, id); session != nil {
		return session, nil
	}
	// A session may have been created since the last scan.
	d.mu.Lock()
	d.cachedAt = time.Time{}
	d.mu.Unlock()
	if session := lookupSession(d.List().Sessions, agent, id); session != nil {
		return session, nil
	}
	return nil, os.ErrNotExist
}

// BuildHandoff renders a header, the compaction summary, every message after it
// and a short instruction for the next agent.
func BuildHandoff(session Session, messages []message) string {
	return BuildHandoffTranscript(session, transcript{Messages: messages})
}

// BuildHandoffTranscript prefers the last compaction summary because the agent
// already trimmed that text to fit a context window. Everything the agent said
// after the compaction is included as-is: the compaction itself is what bounds
// the context, so clipping it a second time would drop usable work.
func BuildHandoffTranscript(session Session, data transcript) string {
	summary := strings.TrimSpace(data.Summary)

	const summaryHeading = "## 上次压缩摘要\n\n"
	const messagesHeading = "## 压缩后的对话\n\n"
	const recentHeading = "## 对话\n\n"

	footer := "\n## 请接手\n\n以上是上一个会话的上下文。请在此基础上继续，不要重复已完成的工作；如果信息不足，先说明你需要的文件或确认点。\n"

	var builder strings.Builder
	builder.WriteString(handoffHeader(session, summary != "", data.SummaryAt))
	if summary != "" {
		builder.WriteString(summaryHeading)
		builder.WriteString(summary)
		builder.WriteString("\n\n")
	}

	switch {
	case len(data.Messages) > 0:
		if summary != "" {
			builder.WriteString(messagesHeading)
		} else {
			builder.WriteString(recentHeading)
		}
	case summary != "":
		builder.WriteString("（压缩后没有新的对话。）\n\n")
	default:
		builder.WriteString("（没有可用的对话文本；该会话可能只包含工具调用或附件。）\n")
	}
	for _, item := range data.Messages {
		builder.WriteString(renderMessage(item))
	}
	builder.WriteString(footer)
	return builder.String()
}

func handoffHeader(session Session, hasSummary bool, summaryAt string) string {
	var builder strings.Builder
	builder.WriteString("# 会话交接：" + nonEmpty(session.Title, session.ID) + "\n\n")
	builder.WriteString("- 来源：" + agentLabel(session.Agent) + "（" + session.ID + "）\n")
	if session.Cwd != "" {
		builder.WriteString("- 工作目录：" + session.Cwd + "\n")
	}
	if session.Updated != "" {
		builder.WriteString("- 最后更新：" + session.Updated + "\n")
	}
	if hasSummary {
		if summaryAt != "" {
			builder.WriteString("- 上次压缩：" + summaryAt + "\n")
		}
		builder.WriteString("- 说明：保留最近一次压缩摘要，并附上压缩后的全部对话。\n\n")
	} else {
		builder.WriteString("- 说明：该会话还没有压缩记录，以下为完整对话。\n\n")
	}
	return builder.String()
}

func renderMessage(item message) string {
	return "**" + item.Role + "**：" + item.Text + "\n\n"
}

func lookupSession(items []Session, agent, id string) *Session {
	for index := range items {
		if items[index].Agent == agent && items[index].ID == id {
			return &items[index]
		}
	}
	return nil
}

func sortSessions(items []Session) {
	for index := 1; index < len(items); index++ {
		for cursor := index; cursor > 0; cursor-- {
			left, right := items[cursor-1], items[cursor]
			if sessionTime(left).After(sessionTime(right)) || sessionTime(left).Equal(sessionTime(right)) {
				break
			}
			items[cursor-1], items[cursor] = items[cursor], items[cursor-1]
		}
	}
}

func sessionTime(session Session) time.Time {
	if session.Updated == "" {
		return time.Time{}
	}
	value, err := time.Parse(time.RFC3339, session.Updated)
	if err != nil {
		return time.Time{}
	}
	return value
}

func appendTurn(list []message, role, text string, _ bool) []message {
	return appendMessage(list, role, text)
}

func appendMessage(list []message, role, text string) []message {
	text = cleanText(text)
	if text == "" {
		return list
	}
	return append(list, message{Role: role, Text: text})
}

func cleanSummary(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSpace(value)
}

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

func titleFromText(value string, limit int) string {
	value = cleanText(value)
	value = strings.Join(strings.Fields(value), " ")
	return truncateRunes(value, limit)
}

func agentLabel(agent string) string {
	switch agent {
	case "opencode":
		return "OpenCode"
	case "qoder":
		return "Qoder"
	case "qoder-cn":
		return "Qoder CN"
	case "workbuddy":
		return "WorkBuddy"
	case "zcode":
		return "ZCode"
	}
	return agent
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func joinHome(home string, parts ...string) string {
	if home == "" {
		return ""
	}
	all := append([]string{home}, parts...)
	return strings.Join(all, string(os.PathSeparator))
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isFile(path string) bool { return fileExists(path) }

func isDir(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (d *Detector) prettyPath(value string) string {
	if value == "" || d.home == "" {
		return value
	}
	relative, err := filepath.Rel(d.home, value)
	if err != nil || strings.HasPrefix(relative, "..") {
		return value
	}
	return "~" + string(os.PathSeparator) + relative
}
