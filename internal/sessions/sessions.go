// Package sessions discovers the local session stores of coding agents (OpenCode,
// Qoder, Qoder CN, WorkBuddy, ZCode) and turns the session you were stuck in into
// a handoff transcript for the next agent.
//
// The design goal is the escape hatch, not the archive: at the moment a session
// dies (quota exhausted, upstream error, interrupted) you need (a) to see which
// session broke and why without opening it, (b) to choose how much context to
// carry, and (c) to paste something the next agent can actually continue from.
package sessions

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const cacheTTL = 5 * time.Second

// MessageLimit bounds how many turns the viewer receives. The handoff builder
// is not limited.
const MessageLimit = 400

// Status is why a session is (or is not) resumable.
type Status struct {
	// State is stuck, active or done.
	State string `json:"state"`
	// Code is a stable machine-readable reason: free-limit, auth, upstream,
	// transport, interrupted, timeout, unknown.
	Code string `json:"code,omitempty"`
	// Label is the short human reason: 额度用完, 上游报错, 被中断…
	Label string `json:"label,omitempty"`
	// Detail is the provider's own message, when it left one.
	Detail string `json:"detail,omitempty"`
	// At is when the failure happened.
	At string `json:"at,omitempty"`
}

// Stuck reports whether the session died rather than finished.
func (s Status) Stuck() bool { return s.State == "stuck" }

// Active reports whether the session is still working.
func (s Status) Active() bool { return s.State == "active" }

// Session is one conversation found in an agent's local store.
type Session struct {
	Agent   string `json:"agent"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Cwd     string `json:"cwd"`
	Updated string `json:"updated"`
	Created string `json:"created,omitempty"`
	Source  string `json:"source"`
	Status  Status `json:"status"`

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

// Turn is one readable message.
type Turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Todo is one entry of an agent's task list.
type Todo struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

// Section describes one copyable block, so the UI can show its real size
// before the user decides whether to carry it.
type Section struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Chars int    `json:"chars"`
	Count int    `json:"count"`
	Note  string `json:"note,omitempty"`
}

// DetailResult is the payload of GET /api/session.
//
// Messages holds only what followed the last compaction; BeforeCompaction is
// how many turns the summary replaced.
type DetailResult struct {
	Agent            string    `json:"agent"`
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	Cwd              string    `json:"cwd,omitempty"`
	Updated          string    `json:"updated,omitempty"`
	Status           Status    `json:"status"`
	Summary          string    `json:"summary,omitempty"`
	SummaryAt        string    `json:"summaryAt,omitempty"`
	Todos            []Todo    `json:"todos"`
	Messages         []Turn    `json:"messages"`
	BeforeCompaction int       `json:"beforeCompaction"`
	TotalMessages    int       `json:"totalMessages"`
	Truncated        bool      `json:"truncated,omitempty"`
	Sections         []Section `json:"sections"`
	Chars            int       `json:"chars"`
}

// transcript is the readable context of one session: the last compaction
// summary an agent stored (when available) and every message after it.
type transcript struct {
	Summary   string
	SummaryAt string
	Messages  []Turn
	// BeforeCompaction counts the turns the summary replaced.
	BeforeCompaction int
	Todos            []Todo
	Status           Status
}

// Detector scans the local agent stores. It is safe for concurrent use.
type Detector struct {
	home string

	mu       sync.Mutex
	cache    ListResult
	cachedAt time.Time
}

// NewDetector resolves the local home.
func NewDetector() *Detector {
	home, _ := os.UserHomeDir()
	return &Detector{home: home}
}

// Home exposes the resolved home directory for path shortening.
func (d *Detector) Home() string { return d.home }

// List returns every session found in the local agent stores, stuck first.
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
	items, err = d.qoderSessions("qoder", "Qoder", joinHome(d.home, ".qoder", "projects"))
	merge(items, err, "Qoder")
	items, err = d.qoderSessions("qoder-cn", "Qoder CN", joinHome(d.home, ".qoder-cn", "projects"))
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

// Detail returns the readable state of one session for the viewer.
func (d *Detector) Detail(agent, id string) (DetailResult, error) {
	session, err := d.find(agent, id)
	if err != nil {
		return DetailResult{}, err
	}

	data, err := d.readTranscript(*session, false)
	if err != nil {
		return DetailResult{}, err
	}

	result := DetailResult{
		Agent: session.Agent, ID: session.ID, Title: session.Title,
		Cwd: session.Cwd, Updated: session.Updated, Status: data.Status,
		Summary: data.Summary, SummaryAt: data.SummaryAt,
		Todos: data.Todos, BeforeCompaction: data.BeforeCompaction,
		TotalMessages: len(data.Messages),
	}
	messages := data.Messages
	if len(messages) > MessageLimit {
		result.Truncated = true
		messages = messages[len(messages)-MessageLimit:]
	}
	result.Messages = messages
	result.Sections = buildSections(data)
	for _, section := range result.Sections {
		result.Chars += section.Chars
	}
	return result, nil
}

// HandoffOptions selects which blocks go into the transcript.
type HandoffOptions struct {
	// Note is the user's own "what I was doing / what comes next" sentence.
	Note string
	// Reason overrides the detected reason when the user picks another one.
	Reason string
	// Summary, Messages and Todos select the blocks; Full means the whole
	// conversation rather than only what followed the last compaction.
	Summary  bool
	Messages bool
	Todos    bool
	Full     bool
	// Limit clips the messages block to that many characters. Zero means no
	// clipping.
	Limit int
}

// HandoffResult is the payload of POST /api/handoff.
type HandoffResult struct {
	Agent    string    `json:"agent"`
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Text     string    `json:"text"`
	Chars    int       `json:"chars"`
	Sections []Section `json:"sections"`
	Status   Status    `json:"status"`
	Clipped  bool      `json:"clipped,omitempty"`
	Limit    int       `json:"limit,omitempty"`
}

// Handoff builds the handoff transcript for one session.
func (d *Detector) Handoff(agent, id string, options HandoffOptions) (HandoffResult, error) {
	session, err := d.find(agent, id)
	if err != nil {
		return HandoffResult{}, err
	}
	data, err := d.readTranscript(*session, options.Full)
	if err != nil {
		return HandoffResult{}, err
	}

	text, _, clipped := BuildHandoff(*session, data, options)
	return HandoffResult{
		Agent: session.Agent, ID: session.ID, Title: session.Title,
		Text: text, Chars: utf8.RuneCountInString(text),
		Sections: buildSections(data), Status: data.Status,
		Clipped: clipped, Limit: options.Limit,
	}, nil
}

func (d *Detector) readTranscript(session Session, full bool) (transcript, error) {
	switch session.kind {
	case "opencode":
		return d.opencodeTranscript(session, full)
	case "qoder":
		return qoderTranscript(session.file, full, session.Status)
	case "workbuddy":
		return workbuddyTranscript(session.file, full, session.Status)
	case "zcode":
		return zcodeTranscript(session.file, full, session.Status)
	case "zcode-db":
		return zcodeDBTranscript(session.file, session.ID, full, session.Status)
	default:
		return transcript{}, errors.New("未知的会话来源")
	}
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

func buildSections(data transcript) []Section {
	sections := make([]Section, 0, 4)
	summary := Section{Key: "summary", Label: "上次压缩摘要"}
	if data.Summary != "" {
		summary.Chars = utf8.RuneCountInString(data.Summary)
	} else {
		summary.Note = "该会话没有压缩记录"
	}
	sections = append(sections, summary)

	messages := Section{
		Key:   "messages",
		Label: "对话",
		Chars: turnsChars(data.Messages),
		Count: len(data.Messages),
	}
	// 有压缩边界时才提示「压缩后的对话」，否则就是整段对话。
	if data.BeforeCompaction > 0 {
		messages.Label = "压缩后的对话"
		messages.Note = beforeNote(data.BeforeCompaction)
	}
	sections = append(sections, messages)

	// 待办块始终出现：没有待办时写明「没有」，比少一行更好解释。
	todos := Section{Key: "todos", Label: "未完成待办"}
	if pending := pendingTodos(data.Todos); len(pending) > 0 {
		todos.Chars = todoChars(pending)
		todos.Count = len(pending)
	} else {
		todos.Note = "没有未完成的待办"
	}
	sections = append(sections, todos)
	return sections
}

func beforeNote(count int) string {
	if count <= 0 {
		return ""
	}
	return "压缩前的 " + strconv.Itoa(count) + " 条已由摘要覆盖"
}

func turnsChars(turns []Turn) int {
	total := 0
	for _, turn := range turns {
		total += utf8.RuneCountInString(turn.Text)
	}
	return total
}

func todoChars(todos []Todo) int {
	total := 0
	for _, todo := range todos {
		total += utf8.RuneCountInString(todo.Text)
	}
	return total
}

func pendingTodos(todos []Todo) []Todo {
	out := make([]Todo, 0, len(todos))
	for _, todo := range todos {
		if todo.Status == "completed" || todo.Status == "cancelled" {
			continue
		}
		out = append(out, todo)
	}
	return out
}

func lookupSession(items []Session, agent, id string) *Session {
	for index := range items {
		if items[index].Agent == agent && items[index].ID == id {
			return &items[index]
		}
	}
	return nil
}

// sortSessions puts stuck sessions first, then orders by recency.
func sortSessions(items []Session) {
	for index := 1; index < len(items); index++ {
		for cursor := index; cursor > 0; cursor-- {
			// Swap only while the right-hand item belongs further up.
			if !before(items[cursor], items[cursor-1]) {
				break
			}
			items[cursor-1], items[cursor] = items[cursor], items[cursor-1]
		}
	}
}

// before reports whether left should be listed ahead of right.
func before(left, right Session) bool {
	if left.Status.Stuck() != right.Status.Stuck() {
		return left.Status.Stuck()
	}
	return sessionTime(left).After(sessionTime(right))
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
