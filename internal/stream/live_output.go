package stream

import (
	"sync"
	"time"

	"github.com/Schallophy/literouter/internal/model"
)

const (
	defaultMaxSessions  = 50
	defaultMaxTextChars = 32_000
	defaultMaxRawChars  = 128_000
	maxToolCalls        = 8
	maxToolCallArgs     = 4_000
	truncatedMarker     = "\n\n…（输出过长，已截断）"
	rawTruncatedMarker  = "\n\n…（原始响应过长，已截断）"
)

// LiveOutputStatus is the lifecycle state of one live session.
type LiveOutputStatus string

const (
	LiveStreaming LiveOutputStatus = "streaming"
	LiveOK        LiveOutputStatus = "ok"
	LiveError     LiveOutputStatus = "error"
)

// LiveOutputSession is the full in-memory session record.
type LiveOutputSession struct {
	ID           int64            `json:"id"`
	At           string           `json:"at"`
	API          model.ApiKind    `json:"api"`
	Model        string           `json:"model"`
	Provider     *string          `json:"provider"`
	Caller       string           `json:"caller"`
	Requested    string           `json:"requested"`
	Stream       bool             `json:"stream"`
	Status       LiveOutputStatus `json:"status"`
	Text         string           `json:"text"`
	ToolCalls    []OutputToolCall `json:"toolCalls"`
	FinishReason *string          `json:"finishReason"`
	Raw          string           `json:"raw"`
	RawTruncated bool             `json:"rawTruncated"`
	Usage        *model.Usage     `json:"usage"`
	Bytes        int64            `json:"bytes"`
	LatencyMs    int64            `json:"latencyMs"`
	Failovers    int              `json:"failovers"`
	Error        *string          `json:"error"`
	Truncated    bool             `json:"truncated"`
}

// LiveOutputView is the API-facing snapshot; raw bytes are replaced by length.
type LiveOutputView struct {
	ID           int64            `json:"id"`
	At           string           `json:"at"`
	API          model.ApiKind    `json:"api"`
	Model        string           `json:"model"`
	Provider     *string          `json:"provider"`
	Caller       string           `json:"caller"`
	Requested    string           `json:"requested"`
	Stream       bool             `json:"stream"`
	Status       LiveOutputStatus `json:"status"`
	Text         string           `json:"text"`
	ToolCalls    []OutputToolCall `json:"toolCalls"`
	FinishReason *string          `json:"finishReason"`
	RawLength    int              `json:"rawLength"`
	RawTruncated bool             `json:"rawTruncated"`
	Usage        *model.Usage     `json:"usage"`
	Bytes        int64            `json:"bytes"`
	LatencyMs    int64            `json:"latencyMs"`
	Failovers    int              `json:"failovers"`
	Error        *string          `json:"error"`
	Truncated    bool             `json:"truncated"`
}

// LiveOutputStart seeds a session.
type LiveOutputStart struct {
	API       model.ApiKind
	Model     string
	Provider  *string
	Caller    string
	Requested string
	Stream    bool
	Failovers int
}

// LiveOutputFinish patches a session at completion.
type LiveOutputFinish struct {
	Text         *string
	ToolCalls    []OutputToolCall
	FinishReason *string
	Raw          *string
	Usage        *model.Usage
	Bytes        *int64
	LatencyMs    *int64
	Failovers    *int
	Error        *string
}

// LiveOutputLimits is the configured ring capacity.
type LiveOutputLimits struct {
	Sessions  int `json:"sessions"`
	TextChars int `json:"textChars"`
	RawChars  int `json:"rawChars"`
}

// LiveOutputEvent is pushed to dashboard SSE listeners.
type LiveOutputEvent struct {
	Type    string          `json:"type"`
	Session *LiveOutputView `json:"session,omitempty"`
	ID      int64           `json:"id,omitempty"`
	Text    string          `json:"text,omitempty"`
}

// LiveOutputLog is the in-process ring of recent outputs.
type LiveOutputLog struct {
	mu           sync.Mutex
	maxSessions  int
	maxTextChars int
	maxRawChars  int
	sessions     []*LiveOutputSession
	nextID       int64
	listeners    map[int64]func(LiveOutputEvent)
	nextListener int64
}

// NewLiveOutputLog builds a log; zero values take the defaults.
func NewLiveOutputLog(maxSessions, maxTextChars, maxRawChars int) *LiveOutputLog {
	if maxSessions <= 0 {
		maxSessions = defaultMaxSessions
	}
	if maxTextChars <= 0 {
		maxTextChars = defaultMaxTextChars
	}
	if maxRawChars <= 0 {
		maxRawChars = defaultMaxRawChars
	}
	log := &LiveOutputLog{
		maxSessions:  maxSessions,
		maxTextChars: maxTextChars,
		maxRawChars:  maxRawChars,
		nextID:       1,
		listeners:    map[int64]func(LiveOutputEvent){},
	}
	return log
}

// Start creates a session and returns its handle.
func (l *LiveOutputLog) Start(input LiveOutputStart) *LiveOutputHandle {
	l.mu.Lock()
	session := &LiveOutputSession{
		ID:        l.nextID,
		At:        time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		API:       input.API,
		Model:     input.Model,
		Provider:  input.Provider,
		Caller:    input.Caller,
		Requested: input.Requested,
		Stream:    input.Stream,
		Status:    LiveStreaming,
		ToolCalls: []OutputToolCall{},
		Failovers: input.Failovers,
	}
	l.nextID++
	l.sessions = append(l.sessions, session)
	l.trimLocked()
	view := cloneSession(session)
	l.emitLocked(LiveOutputEvent{Type: "start", Session: &view})
	l.mu.Unlock()
	return &LiveOutputHandle{id: session.ID, log: l, session: session, extractor: NewSSEOutputExtractor(input.API)}
}

// Recent returns up to limit newest sessions.
func (l *LiveOutputLog) Recent(limit int) []LiveOutputView {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > l.maxSessions {
		limit = l.maxSessions
	}
	start := len(l.sessions) - limit
	if start < 0 {
		start = 0
	}
	views := make([]LiveOutputView, 0, len(l.sessions)-start)
	for _, session := range l.sessions[start:] {
		views = append(views, cloneSession(session))
	}
	return views
}

// Limits returns the ring configuration.
func (l *LiveOutputLog) Limits() LiveOutputLimits {
	return LiveOutputLimits{Sessions: l.maxSessions, TextChars: l.maxTextChars, RawChars: l.maxRawChars}
}

// Clear drops every session.
func (l *LiveOutputLog) Clear() {
	l.mu.Lock()
	l.sessions = nil
	l.emitLocked(LiveOutputEvent{Type: "clear"})
	l.mu.Unlock()
}

// Raw returns a session's raw body and truncation flag.
func (l *LiveOutputLog) Raw(id int64) (string, bool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, session := range l.sessions {
		if session.ID == id {
			return session.Raw, session.RawTruncated, true
		}
	}
	return "", false, false
}

// Subscribe registers a listener; the returned function unsubscribes.
func (l *LiveOutputLog) Subscribe(listener func(LiveOutputEvent)) func() {
	l.mu.Lock()
	id := l.nextListener
	l.nextListener++
	l.listeners[id] = listener
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		delete(l.listeners, id)
		l.mu.Unlock()
	}
}

func (l *LiveOutputLog) appendTextLocked(session *LiveOutputSession, text string) {
	if text == "" || session.Truncated {
		return
	}
	remaining := l.maxTextChars - len([]rune(session.Text))
	if remaining <= 0 {
		session.Truncated = true
		session.Text += truncatedMarker
		l.emitLocked(LiveOutputEvent{Type: "delta", ID: session.ID, Text: truncatedMarker})
		return
	}
	runes := []rune(text)
	slice := text
	if len(runes) > remaining {
		slice = string(runes[:remaining])
	}
	session.Text += slice
	l.emitLocked(LiveOutputEvent{Type: "delta", ID: session.ID, Text: slice})
	if len(runes) > remaining {
		session.Truncated = true
		session.Text += truncatedMarker
		l.emitLocked(LiveOutputEvent{Type: "delta", ID: session.ID, Text: truncatedMarker})
	}
}

func (l *LiveOutputLog) captureRawLocked(session *LiveOutputSession, chunk string) {
	if chunk == "" || session.RawTruncated {
		return
	}
	remaining := l.maxRawChars - len(session.Raw)
	if remaining <= 0 {
		session.RawTruncated = true
		session.Raw += rawTruncatedMarker
		return
	}
	slice := chunk
	if len(chunk) > remaining {
		slice = chunk[:remaining]
	}
	session.Raw += slice
	if len(chunk) > remaining {
		session.RawTruncated = true
		session.Raw += rawTruncatedMarker
	}
}

func (l *LiveOutputLog) completeLocked(session *LiveOutputSession, patch LiveOutputFinish) {
	if patch.Text != nil && *patch.Text != "" {
		l.appendTextLocked(session, *patch.Text)
	}
	if patch.Raw != nil && *patch.Raw != "" {
		l.captureRawLocked(session, *patch.Raw)
	}
	status := LiveOK
	if patch.Error != nil {
		status = LiveError
	}
	session.Status = status
	session.ToolCalls = trimToolCalls(patch.ToolCalls)
	session.FinishReason = patch.FinishReason
	session.Usage = patch.Usage
	if patch.Bytes != nil {
		session.Bytes = *patch.Bytes
	}
	if patch.LatencyMs != nil {
		session.LatencyMs = *patch.LatencyMs
	}
	if patch.Failovers != nil {
		session.Failovers = *patch.Failovers
	}
	session.Error = patch.Error
	view := cloneSession(session)
	l.emitLocked(LiveOutputEvent{Type: "done", Session: &view})
	l.trimLocked()
}

func (l *LiveOutputLog) trimLocked() {
	for len(l.sessions) > l.maxSessions {
		index := -1
		for i, session := range l.sessions {
			if session.Status != LiveStreaming {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		l.sessions = append(l.sessions[:index], l.sessions[index+1:]...)
	}
}

func (l *LiveOutputLog) emitLocked(event LiveOutputEvent) {
	for _, listener := range l.listeners {
		func() {
			defer func() { _ = recover() }()
			listener(event)
		}()
	}
}

// LiveOutputHandle streams one response into a session.
type LiveOutputHandle struct {
	mu        sync.Mutex
	id        int64
	log       *LiveOutputLog
	session   *LiveOutputSession
	extractor *SseOutputExtractor
	finished  bool
}

// ID returns the session id.
func (h *LiveOutputHandle) ID() int64 { return h.id }

// Push feeds raw SSE text; visible deltas are parsed and raw bytes retained.
func (h *LiveOutputHandle) Push(chunk string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished {
		return
	}
	h.log.mu.Lock()
	h.log.captureRawLocked(h.session, chunk)
	h.log.mu.Unlock()
	text := h.extractor.Push(chunk)
	if text != "" {
		h.log.mu.Lock()
		h.log.appendTextLocked(h.session, text)
		h.log.mu.Unlock()
	}
}

// FinishReason reports the protocol finish reason.
func (h *LiveOutputHandle) FinishReason() *string { return h.extractor.FinishReason() }

// ToolCalls returns accumulated tool calls.
func (h *LiveOutputHandle) ToolCalls() []OutputToolCall { return h.extractor.ToolCalls() }

// Finish closes the session, flushing any trailing block.
func (h *LiveOutputHandle) Finish(patch LiveOutputFinish) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.finished {
		return
	}
	h.finished = true
	tail := h.extractor.Flush()
	h.log.mu.Lock()
	defer h.log.mu.Unlock()
	if tail != "" {
		h.log.appendTextLocked(h.session, tail)
	}
	if patch.ToolCalls == nil {
		patch.ToolCalls = h.extractor.ToolCalls()
	}
	if patch.FinishReason == nil {
		patch.FinishReason = h.extractor.FinishReason()
	}
	h.log.completeLocked(h.session, patch)
}

func cloneSession(session *LiveOutputSession) LiveOutputView {
	calls := make([]OutputToolCall, len(session.ToolCalls))
	copy(calls, session.ToolCalls)
	var usage *model.Usage
	if session.Usage != nil {
		copied := *session.Usage
		usage = &copied
	}
	return LiveOutputView{
		ID:           session.ID,
		At:           session.At,
		API:          session.API,
		Model:        session.Model,
		Provider:     copyString(session.Provider),
		Caller:       session.Caller,
		Requested:    session.Requested,
		Stream:       session.Stream,
		Status:       session.Status,
		Text:         session.Text,
		ToolCalls:    calls,
		FinishReason: copyString(session.FinishReason),
		RawLength:    len(session.Raw),
		RawTruncated: session.RawTruncated,
		Usage:        usage,
		Bytes:        session.Bytes,
		LatencyMs:    session.LatencyMs,
		Failovers:    session.Failovers,
		Error:        copyString(session.Error),
		Truncated:    session.Truncated,
	}
}

func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func trimToolCalls(calls []OutputToolCall) []OutputToolCall {
	if len(calls) > maxToolCalls {
		calls = calls[:maxToolCalls]
	}
	trimmed := make([]OutputToolCall, 0, len(calls))
	for _, call := range calls {
		args := call.Arguments
		if len([]rune(args)) > maxToolCallArgs {
			args = string([]rune(args)[:maxToolCallArgs]) + "…"
		}
		trimmed = append(trimmed, OutputToolCall{Name: call.Name, Arguments: args})
	}
	return trimmed
}
