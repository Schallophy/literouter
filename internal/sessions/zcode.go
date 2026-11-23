package sessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ---- ZCode -----------------------------------------------------------------
//
// ZCode keeps its full history in SQLite (db.sqlite) and, in older builds,
// one rollout JSONL per session whose records are whole API requests.

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type zcodeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type zcodeRecord struct {
	CompletedAt string `json:"completedAt"`
	SessionID   string `json:"sessionId"`
	Request     struct {
		Body struct {
			System   json.RawMessage `json:"system"`
			Messages []zcodeMessage  `json:"messages"`
		} `json:"body"`
		Messages []zcodeMessage `json:"messages"`
	} `json:"request"`
	Error *struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"error"`
}

func (d *Detector) zcodeSessions() ([]Session, error) {
	db := zcodeDBPath(d.home)
	if fileExists(db) {
		items, err := zcodeDBSessions(db)
		if err == nil {
			return items, nil
		}
		// A locked or half-checkpointed database should not hide the sessions
		// that still have rollout logs.
		if fallback, _ := d.zcodeRolloutSessions(joinHome(d.home, ".zcode", "cli", "rollout")); len(fallback) > 0 {
			return fallback, nil
		}
		return nil, err
	}
	return d.zcodeRolloutSessions(joinHome(d.home, ".zcode", "cli", "rollout"))
}

func (d *Detector) zcodeRolloutSessions(base string) ([]Session, error) {
	if !isDir(base) {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(base, "model-io-sess_*.jsonl"))
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(matches))
	for _, file := range matches {
		record, err := lastZcodeRecord(file)
		if err != nil || record == nil {
			continue
		}
		id := record.SessionID
		if id == "" {
			id = strings.TrimPrefix(strings.TrimSuffix(filepath.Base(file), ".jsonl"), "model-io-sess_")
		}
		// The newest record only carries the tail of the conversation, so the
		// title and the working directory come from the first full record.
		summary := record
		if first := firstZcodeRecord(file); first != nil {
			summary = first
		}
		title, cwd := zcodeSummary(summary)
		status := Status{State: "done"}
		if record.Error != nil {
			status = classify(record.Error.Type+record.Error.Name, record.Error.Message)
			status.At = record.CompletedAt
		}
		out = append(out, Session{
			Agent: "zcode", ID: id, Title: nonEmpty(title, id), Cwd: cwd,
			Updated: record.CompletedAt, Source: file, Status: status,
			kind: "zcode", file: file,
		})
	}
	return out, nil
}

func zcodeSummary(record *zcodeRecord) (string, string) {
	cwd := ""
	if match := zcodeCwdPattern.FindStringSubmatch(zcodeSystemText(record.Request.Body.System)); len(match) == 2 {
		cwd = strings.TrimSpace(match[1])
	}
	rows := zcodeRows(record)
	for _, row := range rows {
		if cwd != "" {
			break
		}
		if match := zcodeCwdPattern.FindStringSubmatch(cleanText(zcodeContent(row.Content))); len(match) == 2 {
			cwd = strings.TrimSpace(match[1])
		}
	}
	for _, row := range rows {
		if row.Role != "user" {
			continue
		}
		if title := titleFromText(zcodeContent(row.Content), 60); title != "" {
			return title, cwd
		}
	}
	return "", cwd
}

func zcodeRows(record *zcodeRecord) []zcodeMessage {
	if len(record.Request.Body.Messages) > 0 {
		return record.Request.Body.Messages
	}
	return record.Request.Messages
}

func zcodeSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var parts []textPart
	if json.Unmarshal(raw, &parts) == nil {
		return joinTextParts(parts)
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

// firstZcodeRecord finds the first request that still holds the beginning of
// the conversation, which is where the title lives.
func firstZcodeRecord(file string) *zcodeRecord {
	fileHandle, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer fileHandle.Close()
	scanner := bufio.NewScanner(io.LimitReader(fileHandle, 64*1024*1024))
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for lines := 0; scanner.Scan() && lines < 8; lines++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record zcodeRecord
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		for _, row := range zcodeRows(&record) {
			if row.Role == "user" && titleFromText(zcodeContent(row.Content), 60) != "" {
				return &record
			}
		}
	}
	return nil
}

func zcodeTranscript(file string, full bool, status Status) (transcript, error) {
	record, err := lastZcodeRecord(file)
	if err != nil {
		return transcript{}, err
	}
	var data transcript
	data.Status = status
	if !full && len(data.Messages) > 0 {
		data.BeforeCompaction = len(data.Messages)
		data.Messages = nil
	}
	for _, row := range zcodeRows(record) {
		switch row.Role {
		case "user":
			data.Messages = appendTurn(data.Messages, "用户", zcodeContent(row.Content))
		case "assistant":
			data.Messages = appendTurn(data.Messages, "助手", zcodeContent(row.Content))
		case "system":
			// Older ZCode builds embed the compaction summary in the system
			// turn; it is the only summary signal available.
			if text := zcodeSystemText(row.Content); text != "" && strings.Contains(text, "## Objective") {
				data.Summary = cleanSummary(text)
			}
		}
	}
	return data, nil
}

func lastZcodeRecord(file string) (*zcodeRecord, error) {
	lines := bytes.Split(readTail(file, 2*1024*1024), []byte{'\n'})
	for index := len(lines) - 1; index >= 0; index-- {
		line := bytes.TrimSpace(lines[index])
		if len(line) == 0 {
			continue
		}
		var record zcodeRecord
		if json.Unmarshal(line, &record) == nil && (record.SessionID != "" || record.CompletedAt != "") {
			return &record, nil
		}
	}
	return nil, errors.New("没有可用记录")
}

func zcodeContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var parts []textPart
	if json.Unmarshal(raw, &parts) == nil {
		return joinTextParts(parts)
	}
	return ""
}

// ---- shared parsing ------------------------------------------------------

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// 直接是 parts 数组：Qoder 的 message.content 就是这种形状。
	var parts []textPart
	if json.Unmarshal(raw, &parts) == nil && len(parts) > 0 {
		return joinTextParts(parts)
	}
	var object struct {
		Content []textPart `json:"content"`
		Text    string     `json:"text"`
	}
	if json.Unmarshal(raw, &object) == nil {
		if len(object.Content) > 0 {
			return joinTextParts(object.Content)
		}
		if object.Text != "" {
			return object.Text
		}
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		if strings.TrimSpace(value) == "" {
			return ""
		}
		var nested struct {
			Content []textPart `json:"content"`
		}
		if json.Unmarshal([]byte(value), &nested) == nil && len(nested.Content) > 0 {
			return joinTextParts(nested.Content)
		}
		return value
	}
	return ""
}

func joinTextParts(parts []textPart) string {
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "", "text", "input_text", "output_text":
		default:
			continue
		}
		if strings.TrimSpace(part.Text) != "" {
			values = append(values, part.Text)
		}
	}
	return strings.Join(values, "\n")
}

func readHead(file string, limit int64) []byte {
	fileHandle, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer fileHandle.Close()
	data, _ := io.ReadAll(io.LimitReader(fileHandle, limit))
	return data
}

func readTail(file string, limit int64) []byte {
	fileHandle, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer fileHandle.Close()
	info, err := fileHandle.Stat()
	if err != nil {
		return nil
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := fileHandle.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	data, _ := io.ReadAll(fileHandle)
	if start > 0 {
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			data = data[index+1:]
		}
	}
	return data
}

// formatTimestamp renders an agent timestamp stored either as epoch
// milliseconds or as an ISO string.
func formatTimestamp(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var millis int64
	if json.Unmarshal(raw, &millis) == nil && millis > 0 {
		return formatMillis(millis)
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	return ""
}
