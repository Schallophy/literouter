package sessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxJSONLine = 16 * 1024 * 1024

var (
	systemReminderPattern = regexp.MustCompile(`(?is)<system-reminder\b[^>]*>.*?</system-reminder>`)
	userInfoPattern       = regexp.MustCompile(`(?is)<user_info\b[^>]*>.*?</user_info>`)
	wrapperTagPattern     = regexp.MustCompile(`(?i)</?(user_query|long_text_quote|long-text|user-context|environment)\b[^>]*>`)
	zcodeCwdPattern       = regexp.MustCompile(`(?m)Primary working directory:\s*(.+)`)
)

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// detectAgents mirrors the install markers used by magpie's detector.
func (d *Detector) detectAgents() []Agent {
	zcodeStore := zcodeDBPath(d.home)
	if !fileExists(zcodeStore) {
		zcodeStore = joinHome(d.home, ".zcode", "cli", "rollout")
	}
	specs := []struct {
		id     string
		label  string
		marker string
		store  string
	}{
		{"opencode", "OpenCode", joinHome(d.home, ".config", "opencode", "opencode.json"), joinHome(d.home, ".local", "share", "opencode", "opencode.db")},
		{"qoder", "Qoder", joinHome(d.home, ".qoder", "settings.json"), joinHome(d.home, ".qoder", "projects")},
		{"qoder-cn", "Qoder CN", joinHome(d.home, ".qoder-cn", "settings.json"), joinHome(d.home, ".qoder-cn", "projects")},
		{"workbuddy", "WorkBuddy", joinHome(d.home, ".workbuddy", "models.json"), joinHome(d.home, ".workbuddy", "projects")},
		{"zcode", "ZCode", joinHome(d.home, ".zcode", "v2", "config.json"), zcodeStore},
	}
	agents := make([]Agent, 0, len(specs))
	for _, spec := range specs {
		present := fileExists(spec.marker) || isDir(spec.store) || fileExists(spec.store)
		agents = append(agents, Agent{
			ID: spec.id, Label: spec.label, Present: present,
			Store: d.prettyPath(spec.store),
		})
	}
	return agents
}

func findOpencodeCLI(home string) string {
	if value := strings.TrimSpace(os.Getenv("LITEROUTER_OPENCODE_CLI")); value != "" && fileExists(value) {
		return value
	}
	var candidates []string
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates,
			filepath.Join(local, "Programs", "ai.opencode.desktop", "resources", "opencode-cli.exe"),
			filepath.Join(local, "Programs", "opencode", "opencode.exe"),
		)
	}
	if roaming := os.Getenv("APPDATA"); roaming != "" {
		matches, _ := filepath.Glob(filepath.Join(roaming, "ai.opencode.desktop", "cli", "*", "opencode-cli.exe"))
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		candidates = append(candidates, matches...)
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".opencode", "bin", "opencode.exe"))
	}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			return candidate
		}
	}
	if path, err := exec.LookPath("opencode"); err == nil {
		return path
	}
	return ""
}

func opencodeRoots(home string) []string {
	if raw := strings.TrimSpace(os.Getenv("LITEROUTER_SESSIONS_ROOT")); raw != "" {
		var roots []string
		for _, part := range strings.FieldsFunc(raw, func(value rune) bool {
			return value == ',' || value == os.PathListSeparator
		}) {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				roots = append(roots, trimmed)
			}
		}
		if len(roots) > 0 {
			return roots
		}
	}
	var roots []string
	seen := map[string]bool{}
	add := func(path string) {
		if path == "" || seen[path] || !isDir(path) {
			return
		}
		seen[path] = true
		roots = append(roots, path)
	}
	if home != "" {
		add(filepath.Join(home, "projects"))
	}
	if working, err := os.Getwd(); err == nil {
		add(working)
	}
	if len(roots) == 0 && home != "" {
		roots = append(roots, home)
	}
	return roots
}

// ---- OpenCode ------------------------------------------------------------

func (d *Detector) opencodeSessions() ([]Session, error) {
	if d.opencodeCLI == "" {
		if fileExists(joinHome(d.home, ".local", "share", "opencode", "opencode.db")) {
			return nil, errors.New("找到 OpenCode 数据，但未找到 opencode CLI（可用 LITEROUTER_OPENCODE_CLI 指定）")
		}
		return nil, nil
	}
	seen := map[string]bool{}
	var out []Session
	var firstErr error
	for _, root := range d.roots {
		if !isDir(root) {
			continue
		}
		items, err := d.listOpencode(root)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, item := range items {
			if seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			out = append(out, item)
		}
	}
	return out, firstErr
}

func (d *Detector) listOpencode(root string) ([]Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, d.opencodeCLI, "session", "list", "--format", "json", "--max-count", "200")
	command.Dir = root
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, errors.New("opencode 会话列表超时")
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, errors.New("opencode 会话列表失败：" + detail)
	}
	var rows []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Updated   int64  `json:"updated"`
		Created   int64  `json:"created"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, errors.New("解析 opencode 会话列表失败：" + err.Error())
	}
	out := make([]Session, 0, len(rows))
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		out = append(out, Session{
			Agent: "opencode", ID: row.ID,
			Title: nonEmpty(row.Title, "(未命名会话)"), Cwd: row.Directory,
			Updated: formatMillis(row.Updated), Created: formatMillis(row.Created),
			Source: joinHome(d.home, ".local", "share", "opencode", "opencode.db"),
			kind:   "opencode",
		})
	}
	return out, nil
}

func (d *Detector) opencodeTranscript(session Session, full bool) (transcript, error) {
	if d.opencodeCLI == "" {
		return transcript{}, errors.New("未找到 opencode CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, d.opencodeCLI, "session", "export", session.ID)
	if session.Cwd != "" && isDir(session.Cwd) {
		command.Dir = session.Cwd
	} else if d.home != "" {
		command.Dir = d.home
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return transcript{}, err
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return transcript{}, err
	}
	data, decodeErr := decodeOpencodeExport(stdout, full)
	waitErr := command.Wait()
	if decodeErr != nil {
		return transcript{}, decodeErr
	}
	if waitErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return transcript{}, errors.New("opencode 会话导出超时")
		}
		return transcript{}, fmt.Errorf("opencode 会话导出失败：%w", waitErr)
	}
	return data, nil
}

// decodeOpencodeExport streams the export JSON and keeps only the context after
// the last completed compaction when full is false, so a huge session does not
// have to fit in memory. The compaction summary already covers everything
// before it.
func decodeOpencodeExport(reader io.Reader, full bool) (transcript, error) {
	decoder := json.NewDecoder(reader)
	token, err := decoder.Token()
	if err != nil {
		return transcript{}, errors.New("读取 opencode 导出失败：" + err.Error())
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return transcript{}, errors.New("opencode 导出格式不正确")
	}
	var data transcript
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return transcript{}, err
		}
		key, _ := keyToken.(string)
		if key != "messages" {
			var skip json.RawMessage
			if err := decoder.Decode(&skip); err != nil {
				return transcript{}, err
			}
			continue
		}
		open, err := decoder.Token()
		if err != nil {
			return transcript{}, err
		}
		if delimiter, ok := open.(json.Delim); !ok || delimiter != '[' {
			var skip json.RawMessage
			_ = decoder.Decode(&skip)
			continue
		}
		for decoder.More() {
			var row struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Status  string `json:"status"`
				Summary string `json:"summary"`
				Time    struct {
					Created int64 `json:"created"`
				} `json:"time"`
				Content []textPart `json:"content"`
			}
			if err := decoder.Decode(&row); err != nil {
				return transcript{}, err
			}
			switch row.Type {
			case "user":
				data.Messages = appendTurn(data.Messages, "用户", row.Text, full)
			case "assistant":
				data.Messages = appendTurn(data.Messages, "助手", joinTextParts(row.Content), full)
			case "compaction":
				if row.Status != "" && row.Status != "completed" {
					continue
				}
				if summary := cleanSummary(row.Summary); summary != "" {
					data.Summary = summary
					data.SummaryAt = formatMillis(row.Time.Created)
					if !full {
						data.Messages = nil
					}
				}
			}
		}
		if _, err := decoder.Token(); err != nil {
			return transcript{}, err
		}
	}
	return data, nil
}

// ---- Qoder / Qoder CN ----------------------------------------------------

func (d *Detector) qoderSessions(agent, base string) ([]Session, error) {
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
		title, cwd := qoderHead(file)
		updated := ""
		if info, err := os.Stat(file); err == nil {
			updated = info.ModTime().Format(time.RFC3339)
		}
		out = append(out, Session{
			Agent: agent, ID: id, Title: nonEmpty(title, id), Cwd: cwd,
			Updated: updated, Source: file, kind: "qoder", file: file,
		})
	}
	return out, nil
}

func qoderHead(file string) (string, string) {
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
		var row struct {
			Type   string `json:"type"`
			Cwd    string `json:"cwd"`
			Origin struct {
				Kind string `json:"kind"`
			} `json:"origin"`
			HumanInput *struct {
				Text string `json:"text"`
			} `json:"humanInput"`
			Message json.RawMessage `json:"message"`
		}
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
		text := ""
		if row.HumanInput != nil {
			text = row.HumanInput.Text
		}
		if strings.TrimSpace(text) == "" {
			text = contentText(row.Message)
		}
		if title := titleFromText(text, 60); title != "" {
			return title, cwd
		}
	}
	return "", cwd
}

func qoderTranscript(file string, full bool) (transcript, error) {
	fileHandle, err := os.Open(file)
	if err != nil {
		return transcript{}, err
	}
	defer fileHandle.Close()
	scanner := bufio.NewScanner(fileHandle)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLine)
	var data transcript
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row struct {
			Type             string `json:"type"`
			Timestamp        string `json:"timestamp"`
			IsCompactSummary bool   `json:"isCompactSummary"`
			Origin           struct {
				Kind string `json:"kind"`
			} `json:"origin"`
			HumanInput *struct {
				Text string `json:"text"`
			} `json:"humanInput"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		// Qoder injects the last compaction as a user message, flagged with
		// isCompactSummary; everything before it is already summarized.
		if row.IsCompactSummary {
			if summary := cleanSummary(contentText(row.Message)); summary != "" {
				data.Summary = summary
				data.SummaryAt = row.Timestamp
				if !full {
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
			text := ""
			if row.HumanInput != nil {
				text = row.HumanInput.Text
			}
			if strings.TrimSpace(text) == "" {
				text = contentText(row.Message)
			}
			data.Messages = appendTurn(data.Messages, "用户", text, full)
		case "assistant":
			data.Messages = appendTurn(data.Messages, "助手", contentText(row.Message), full)
		}
	}
	return data, nil
}

// ---- WorkBuddy -----------------------------------------------------------

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
		updated := ""
		if info, err := os.Stat(file); err == nil {
			updated = info.ModTime().Format(time.RFC3339)
		}
		out = append(out, Session{
			Agent: "workbuddy", ID: id, Title: nonEmpty(workbuddyTitle(file), id),
			Cwd: workbuddyCwd(file), Updated: updated, Source: file,
			kind: "workbuddy", file: file,
		})
	}
	return out, nil
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

func workbuddyTranscript(file string, full bool) (transcript, error) {
	fileHandle, err := os.Open(file)
	if err != nil {
		return transcript{}, err
	}
	defer fileHandle.Close()
	scanner := bufio.NewScanner(fileHandle)
	scanner.Buffer(make([]byte, 64*1024), maxJSONLine)
	var data transcript
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
		// WorkBuddy stores the compaction result as a user message wrapped in
		// <cb_summary> and marks it with providerData.compactType.
		if row.ProviderData.CompactType != "" || row.ProviderData.IsSummary || strings.Contains(text, "<cb_summary>") {
			if summary := workbuddySummary(text); summary != "" {
				data.Summary = summary
				data.SummaryAt = formatTimestamp(row.Timestamp)
				if !full {
					data.Messages = nil
				}
			}
			continue
		}
		switch row.Role {
		case "user":
			data.Messages = appendTurn(data.Messages, "用户", text, full)
		case "assistant":
			data.Messages = appendTurn(data.Messages, "助手", text, full)
		}
	}
	return data, nil
}

func workbuddySummary(text string) string {
	text = strings.ReplaceAll(text, "<cb_summary>", "")
	text = strings.ReplaceAll(text, "</cb_summary>", "")
	return cleanSummary(text)
}

// ---- ZCode ---------------------------------------------------------------

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
		out = append(out, Session{
			Agent: "zcode", ID: id, Title: nonEmpty(title, id), Cwd: cwd,
			Updated: record.CompletedAt, Source: file, kind: "zcode", file: file,
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

func zcodeTranscript(file string, full bool) ([]message, error) {
	record, err := lastZcodeRecord(file)
	if err != nil {
		return nil, err
	}
	var messages []message
	for _, row := range zcodeRows(record) {
		switch row.Role {
		case "user":
			messages = appendTurn(messages, "用户", zcodeContent(row.Content), full)
		case "assistant":
			messages = appendTurn(messages, "助手", zcodeContent(row.Content), full)
		}
	}
	return messages, nil
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

// ---- shared parsing ------------------------------------------------------

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var object struct {
		Content []textPart `json:"content"`
	}
	if json.Unmarshal(raw, &object) == nil && len(object.Content) > 0 {
		return joinTextParts(object.Content)
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

// formatTimestamp renders an agent timestamp that may be stored either as
// epoch milliseconds or as an ISO string.
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

func formatMillis(value int64) string {
	if value <= 0 {
		return ""
	}
	return time.UnixMilli(value).Format(time.RFC3339)
}
