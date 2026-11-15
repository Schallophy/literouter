package util

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Schallophy/literouter/internal/model"
)

// Logger writes a pretty console line and a JSON line per record.
type Logger struct {
	mu       sync.Mutex
	level    model.LogLevelName
	file     string
	maxBytes int64
	maxFiles int
	pretty   bool
	size     int64
	sized    bool
}

var levelOrder = map[model.LogLevelName]int{
	model.LogDebug: 10,
	model.LogInfo:  20,
	model.LogWarn:  30,
	model.LogError: 40,
}

// ParseLogLevel accepts a level name, defaulting to info.
func ParseLogLevel(value string) model.LogLevelName {
	if model.IsLogLevel(value) {
		return model.LogLevelName(value)
	}
	return model.LogInfo
}

// NewLogger builds a logger.
func NewLogger(level model.LogLevelName, file string, maxBytes int64, maxFiles int) *Logger {
	pretty := false
	if info, err := os.Stdout.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		pretty = true
	}
	if _, exists := os.LookupEnv("NO_COLOR"); exists {
		pretty = false
	}
	if maxBytes < 0 {
		maxBytes = 0
	}
	if maxFiles < 0 {
		maxFiles = 0
	}
	return &Logger{level: level, file: file, maxBytes: maxBytes, maxFiles: maxFiles, pretty: pretty}
}

// NopLogger returns a logger that only respects the level.
func NopLogger() *Logger { return NewLogger(model.LogError, "", 0, 0) }

// SetLevel changes the threshold at runtime.
func (l *Logger) SetLevel(level model.LogLevelName) {
	l.mu.Lock()
	l.level = level
	l.mu.Unlock()
}

// SetRotation changes the rotation policy at runtime.
func (l *Logger) SetRotation(maxBytes int64, maxFiles int) {
	l.mu.Lock()
	if maxBytes < 0 {
		maxBytes = 0
	}
	if maxFiles < 0 {
		maxFiles = 0
	}
	l.maxBytes = maxBytes
	l.maxFiles = maxFiles
	l.mu.Unlock()
}

func (l *Logger) Debug(message string, fields map[string]any) { l.write(model.LogDebug, message, fields) }
func (l *Logger) Info(message string, fields map[string]any)  { l.write(model.LogInfo, message, fields) }
func (l *Logger) Warn(message string, fields map[string]any)  { l.write(model.LogWarn, message, fields) }
func (l *Logger) Error(message string, fields map[string]any) { l.write(model.LogError, message, fields) }

func (l *Logger) write(level model.LogLevelName, message string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if levelOrder[level] < levelOrder[l.level] {
		return
	}
	now := time.Now()
	record := map[string]any{
		"at":      now.UTC().Format("2006-01-02T15:04:05.000Z"),
		"level":   string(level),
		"message": message,
	}
	for key, value := range fields {
		if value != nil {
			record[key] = value
		}
	}
	l.appendLocked(record, now)
	if l.pretty {
		line := fmt.Sprintf("%s %-5s %s%s", now.Format("15:04:05"), strings.ToUpper(string(level)), message, renderFields(fields))
		fmt.Fprintln(os.Stdout, line)
	}
}

// Access records an API request line.
func (l *Logger) Access(method, path string, status int, ms int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if levelOrder[model.LogInfo] < levelOrder[l.level] {
		return
	}
	now := time.Now()
	l.appendLocked(map[string]any{
		"at": now.UTC().Format("2006-01-02T15:04:05.000Z"), "level": "info", "message": "api request",
		"method": method, "path": path, "status": status, "ms": ms,
	}, now)
	if l.pretty && l.level == model.LogDebug {
		fmt.Fprintf(os.Stdout, "%s  %-4s %s  %d %dms\n", now.Format("15:04:05"), method, path, status, ms)
	}
}

func renderFields(fields map[string]any) string {
	if len(fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := fields[key]
		if value == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v", key, value))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

func (l *Logger) appendLocked(record map[string]any, _ time.Time) {
	if l.file == "" {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if !l.sized {
		if info, statErr := os.Stat(l.file); statErr == nil {
			l.size = info.Size()
		}
		l.sized = true
	}
	if l.maxBytes > 0 && l.size > 0 && l.size+int64(len(line)) > l.maxBytes {
		l.rotateLocked()
	}
	_ = os.MkdirAll(filepath.Dir(l.file), 0o755)
	handle, openErr := os.OpenFile(l.file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if openErr != nil {
		l.sized = false
		return
	}
	if _, writeErr := handle.Write(line); writeErr == nil {
		l.size += int64(len(line))
	}
	_ = handle.Close()
}

func (l *Logger) rotateLocked() {
	if l.maxFiles <= 0 {
		_ = os.Remove(l.file)
		l.size = 0
		return
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", l.file, l.maxFiles))
	for index := l.maxFiles - 1; index >= 1; index-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.file, index), fmt.Sprintf("%s.%d", l.file, index+1))
	}
	_ = os.Rename(l.file, l.file+".1")
	l.size = 0
}

// Flush is a no-op; writes are synchronous.
func (l *Logger) Flush() error { return nil }

// Child returns a logger sharing settings (bindings are not needed here).
func (l *Logger) Child(map[string]any) *Logger { return l }
