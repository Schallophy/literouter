package sessions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxJSONLine = 16 * 1024 * 1024

// ---- Qoder / Qoder CN ----------------------------------------------------
//
// One JSONL file per session, grouped by project directory. Compaction arrives
// as a user message flagged isCompactSummary.

func (d *Detector) qoderSessions(agent, label, base string) ([]Session, error) {
	if !isDir(base) {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(base, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(matches))
	for _, file := range matches {
		if !isFile(file) {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(file), ".jsonl")
		if id == "" {
			continue
		}
		title, cwd, status := qoderHead(file)
		updated := ""
		if info, err := os.Stat(file); err == nil {
			updated = modTime(info)
		}
		out = append(out, Session{
			Agent: agent, ID: id, Title: nonEmpty(title, id), Cwd: cwd,
			Updated: updated, Source: file, Status: status,
			kind: "qoder", file: file,
		})
	}
	return out, nil
}

func modTime(info os.FileInfo) string {
	at := info.ModTime()
	if at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339)
}

// qoderHead reads the first page of a session for its title, its working
// directory and how the last turn ended.
//
// The status is decided by the tail, so the whole file is scanned; the head is
// only reused while the tail has not been read yet.
func qoderHead(file string) (string, string, Status) {
	title, cwd := qoderTop(file)
	status := qoderTail(file)
	return title, cwd, status
}

func qoderTop(file string) (string, string) {
	fileHandle, err := os.Open(file)
	if err != nil {
		return "", ""
	}
	defer fileHandle.Close()
	scanner := bufio.NewScanner(fileHandle)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLine)
	cwd := ""
	for lines := 0; scanner.Scan() && lines < 400; lines++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row qoderRow
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		if cwd == "" {
			cwd = row.Cwd
		}
		if row.Type != "user" {
			continue
		}
		if row.Origin.Kind != "" && row.Origin.Kind != "human" {
			continue
		}
		text := qoderUserText(row)
		if title := titleFromText(text, 60); title != "" {
			return title, cwd
		}
	}
	return "", cwd
}

// qoderTail inspects the last few lines for the ending signal.
func qoderTail(file string) Status {
	lines := bytes.Split(readTail(file, 64*1024), []byte{'\n'})
	for index := len(lines) - 1; index >= 0 && index >= len(lines)-6; index-- {
		line := bytes.TrimSpace(lines[index])
		if len(line) == 0 {
			continue
		}
		var row qoderRow
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		if status := qoderStatus(row); status.State != "" {
			return status
		}
	}
	return Status{State: "done"}
}

type qoderRow struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Cwd              string `json:"cwd"`
	Origin           struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	HumanInput *struct {
		Text string `json:"text"`
	} `json:"humanInput"`
	Message json.RawMessage `json:"message"`
}

func qoderUserText(row qoderRow) string {
	text := ""
	if row.HumanInput != nil {
		text = row.HumanInput.Text
	}
	if strings.TrimSpace(text) == "" {
		text = contentText(row.Message)
	}
	return text
}

func qoderStatus(row qoderRow) Status {
	var message struct {
		Error *struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Message string `json:"message"`
		} `json:"error"`
		Finish string `json:"finish"`
	}
	if len(row.Message) > 0 && json.Unmarshal(row.Message, &message) == nil && message.Error != nil {
		status := classify(message.Error.Type+message.Error.Name, message.Error.Message)
		status.At = row.Timestamp
		return status
	}
	if row.Type == "abort" || row.Type == "interrupted" {
		return Status{State: "stuck", Code: "interrupted", Label: "被中断", At: row.Timestamp}
	}
	return Status{}
}

func qoderTranscript(file string, full bool, status Status) (transcript, error) {
	fileHandle, err := os.Open(file)
	if err != nil {
		return transcript{}, err
	}
	defer fileHandle.Close()
	scanner := bufio.NewScanner(fileHandle)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLine)

	var data transcript
	data.Status = status
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row qoderRow
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		// Qoder injects the last compaction as a user message flagged with
		// isCompactSummary; everything before it is already summarized.
		if row.IsCompactSummary {
			if summary := cleanSummary(contentText(row.Message)); summary != "" {
				data.Summary = summary
				data.SummaryAt = row.Timestamp
				if !full {
					// 压缩可以发生多次：每次替换的都是「此前所有对话」，
					// 所以要累加，而不是只记最后一段。
					data.BeforeCompaction += len(data.Messages)
					data.Messages = nil
				}
			}
			continue
		}
		switch row.Type {
		case "user":
			if row.Origin.Kind != "" && row.Origin.Kind != "human" {
				continue
			}
			data.Messages = appendTurn(data.Messages, "用户", qoderUserText(row))
		case "assistant":
			data.Messages = appendTurn(data.Messages, "助手", contentText(row.Message))
		case "todo":
			// Older Qoder builds stored tasks as stream rows.
			var todo struct {
				Content string `json:"content"`
				Status  string `json:"status"`
			}
			if json.Unmarshal(row.Message, &todo) == nil && todo.Content != "" {
				data.Todos = append(data.Todos, Todo{Text: cleanText(todo.Content), Status: todo.Status})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return transcript{}, err
	}
	return data, nil
}

// ---- WorkBuddy -----------------------------------------------------------
//
// One JSONL file per session. Compaction is stored as a user message wrapped in
// <cb_summary> and marked with providerData.compactType.

func (d *Detector) workbuddySessions(base string) ([]Session, error) {
	if !isDir(base) {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(base, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(matches))
	for _, file := range matches {
		name := filepath.Base(file)
		if strings.HasSuffix(name, ".file-rollback.ndjson") || !isFile(file) {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		updated, status := "", Status{State: "done"}
		if info, err := os.Stat(file); err == nil {
			updated = modTime(info)
		}
		if tail := workbuddyTail(file); tail.State != "" {
			status = tail
		}
		out = append(out, Session{
			Agent: "workbuddy", ID: id, Title: nonEmpty(workbuddyTitle(file), id),
			Cwd: workbuddyCwd(file), Updated: updated, Source: file, Status: status,
			kind: "workbuddy", file: file,
		})
	}
	return out, nil
}

func workbuddyTail(file string) Status {
	lines := bytes.Split(readTail(file, 64*1024), []byte{'\n'})
	for index := len(lines) - 1; index >= 0 && index >= len(lines)-6; index-- {
		line := bytes.TrimSpace(lines[index])
		if len(line) == 0 {
			continue
		}
		var row struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Error   *struct {
				Type    string `json:"type"`
				Name    string `json:"name"`
				Message string `json:"message"`
			} `json:"error"`
			Finish string `json:"finish"`
		}
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		if row.Type == "error" || row.Error != nil {
			kind, detail := "", ""
			if row.Error != nil {
				kind, detail = row.Error.Type+row.Error.Name, row.Error.Message
			}
			return classify(kind, detail)
		}
		if row.Finish == "error" {
			return classify("", "")
		}
	}
	return Status{}
}

func workbuddyTitle(file string) string {
	for _, line := range bytes.Split(readTail(file, 256*1024), []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row struct {
			Type    string `json:"type"`
			AITitle string `json:"aiTitle"`
		}
		if json.Unmarshal(line, &row) == nil && row.Type == "ai-title" {
			if title := titleFromText(row.AITitle, 60); title != "" {
				return title
			}
		}
	}
	for _, line := range bytes.Split(readHead(file, 256*1024), []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(line, &row) == nil && row.Type == "message" && row.Role == "user" {
			if title := titleFromText(workbuddyContent(row.Content), 60); title != "" {
				return title
			}
		}
	}
	return ""
}

func workbuddyCwd(file string) string {
	for _, line := range bytes.Split(readHead(file, 128*1024), []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &row) == nil && row.Cwd != "" {
			return row.Cwd
		}
	}
	return ""
}

func workbuddyTranscript(file string, full bool, status Status) (transcript, error) {
	fileHandle, err := os.Open(file)
	if err != nil {
		return transcript{}, err
	}
	defer fileHandle.Close()
	scanner := bufio.NewScanner(fileHandle)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLine)

	var data transcript
	data.Status = status
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row struct {
			Type         string          `json:"type"`
			Role         string          `json:"role"`
			Timestamp    json.RawMessage `json:"timestamp"`
			Content      json.RawMessage `json:"content"`
			ProviderData struct {
				CompactType string `json:"compactType"`
				IsSummary   bool   `json:"isSummary"`
			} `json:"providerData"`
		}
		if json.Unmarshal(line, &row) != nil || row.Type != "message" {
			continue
		}
		text := workbuddyContent(row.Content)
		if row.ProviderData.CompactType != "" || row.ProviderData.IsSummary || strings.Contains(text, "<cb_summary>") {
			if summary := workbuddySummary(text); summary != "" {
				data.Summary = summary
				data.SummaryAt = formatTimestamp(row.Timestamp)
				if !full {
					data.BeforeCompaction += len(data.Messages)
					data.Messages = nil
				}
			}
			continue
		}
		switch row.Role {
		case "user":
			data.Messages = appendTurn(data.Messages, "用户", text)
		case "assistant":
			data.Messages = appendTurn(data.Messages, "助手", text)
		}
	}
	if err := scanner.Err(); err != nil {
		return transcript{}, err
	}
	return data, nil
}

func workbuddySummary(text string) string {
	text = strings.ReplaceAll(text, "<cb_summary>", "")
	text = strings.ReplaceAll(text, "</cb_summary>", "")
	return cleanSummary(text)
}

func workbuddyContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		if strings.TrimSpace(value) == "" {
			return ""
		}
		var parts []textPart
		if json.Unmarshal([]byte(value), &parts) == nil {
			return joinTextParts(parts)
		}
		return value
	}
	var parts []textPart
	if json.Unmarshal(raw, &parts) == nil {
		return joinTextParts(parts)
	}
	return ""
}
