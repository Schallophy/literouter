package stream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EventKind names the event classes written to events.jsonl.
type EventKind string

const (
	EventStart    EventKind = "start"
	EventFailover EventKind = "failover"
	EventDead     EventKind = "dead"
	EventThrottle EventKind = "throttle"
	EventRevive   EventKind = "revive"
	EventCatalog  EventKind = "catalog"
	EventConfig   EventKind = "config"
	EventError    EventKind = "error"
)

// EventLevel is the display severity.
type EventLevel string

const (
	LevelInfo  EventLevel = "info"
	LevelWarn  EventLevel = "warn"
	LevelError EventLevel = "error"
)

// AppEvent is one line of events.jsonl and one dashboard item.
type AppEvent struct {
	ID      int64          `json:"id"`
	At      string         `json:"at"`
	Kind    EventKind      `json:"kind"`
	Level   EventLevel     `json:"level"`
	Message string         `json:"message"`
	Model   *string        `json:"model,omitempty"`
	Chain   *string        `json:"chain,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// EventInput is an event before id/timestamp assignment.
type EventInput struct {
	Kind    EventKind
	Level   EventLevel
	Message string
	Model   *string
	Chain   *string
	Detail  map[string]any
}

// EventLog keeps a bounded in-memory ring and appends each event to disk.
type EventLog struct {
	mu       sync.Mutex
	file     string
	capacity int
	items    []AppEvent
	nextID   int64
}

// NewEventLog creates a log; capacity defaults to 400.
func NewEventLog(file string, capacity int) *EventLog {
	if capacity <= 0 {
		capacity = 400
	}
	return &EventLog{file: file, capacity: capacity, nextID: 1}
}

// Push records an event, trims the ring and appends one JSON line.
func (l *EventLog) Push(input EventInput) AppEvent {
	event := AppEvent{
		ID:      l.nextID,
		At:      time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Kind:    input.Kind,
		Level:   input.Level,
		Message: input.Message,
		Model:   input.Model,
		Chain:   input.Chain,
		Detail:  input.Detail,
	}
	l.mu.Lock()
	l.nextID++
	l.items = append(l.items, event)
	if len(l.items) > l.capacity {
		l.items = append([]AppEvent(nil), l.items[len(l.items)-l.capacity:]...)
	}
	file := l.file
	l.mu.Unlock()

	if file != "" {
		if line, err := json.Marshal(event); err == nil {
			_ = os.MkdirAll(filepath.Dir(file), 0o755)
			if handle, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				_, _ = handle.Write(append(line, '\n'))
				_ = handle.Close()
			}
		}
	}
	return event
}

// Recent returns the newest events first, optionally filtered by kind.
func (l *EventLog) Recent(limit int, kind EventKind) []AppEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 {
		limit = 120
	}
	source := l.items
	if kind != "" {
		filtered := make([]AppEvent, 0, len(source))
		for _, item := range source {
			if item.Kind == kind {
				filtered = append(filtered, item)
			}
		}
		source = filtered
	}
	start := len(source) - limit
	if start < 0 {
		start = 0
	}
	out := make([]AppEvent, 0, len(source)-start)
	for index := len(source) - 1; index >= start; index-- {
		out = append(out, source[index])
	}
	return out
}

// Flush is a no-op kept for graceful shutdown symmetry: appends are synchronous.
func (l *EventLog) Flush() error { return nil }
