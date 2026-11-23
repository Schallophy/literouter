package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// OpenCode keeps every session in one SQLite database (opencode.db). Reading it
// directly instead of shelling out to `opencode session export` is deliberate:
// the moment a session dies is exactly when its CLI is least reliable.

// opencodeDBPath returns the SQLite store OpenCode keeps its sessions in.
func opencodeDBPath(home string) string {
	return joinHome(home, ".local", "share", "opencode", "opencode.db")
}

// openOpencodeDB opens the store read-only so we never block or mutate a
// running agent.
func openOpencodeDB(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(3000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (d *Detector) opencodeSessions() ([]Session, error) {
	path := opencodeDBPath(d.home)
	if !fileExists(path) {
		return nil, nil
	}
	db, err := openOpencodeDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The status needs the order of events, not just the newest of each kind:
	// a session can fail, take a new user message and start running again, and
	// calling that "stuck" would send the user to the wrong row. Sequence
	// numbers are compared in opencodeStatus.
	query := `
		SELECT s.id, COALESCE(s.title, ''), COALESCE(s.directory, ''),
		       COALESCE((SELECT x.data FROM session_message x
		                 WHERE x.session_id = s.id AND x.type = 'idle'
		                 ORDER BY x.seq DESC LIMIT 1), ''),
		       COALESCE((SELECT x.seq FROM session_message x
		                 WHERE x.session_id = s.id AND x.type = 'idle'
		                 ORDER BY x.seq DESC LIMIT 1), -1),
		       COALESCE((SELECT y.data FROM session_message y
		                 WHERE y.session_id = s.id AND y.type = 'assistant'
		                 ORDER BY y.seq DESC LIMIT 1), ''),
		       COALESCE((SELECT y.seq FROM session_message y
		                 WHERE y.session_id = s.id AND y.type = 'assistant'
		                 ORDER BY y.seq DESC LIMIT 1), -1),
		       COALESCE((SELECT u.seq FROM session_message u
		                 WHERE u.session_id = s.id AND u.type = 'user'
		                 ORDER BY u.seq DESC LIMIT 1), -1),
		       s.time_created, s.time_updated
		FROM ` + opencodeSessionTable(db) + ` s
		WHERE COALESCE(s.time_archived, 0) = 0
		ORDER BY s.time_updated DESC`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Session, 0, 64)
	for rows.Next() {
		var id, title, directory, idleJSON, assistantJSON string
		var idleSeq, assistantSeq, userSeq, created, updated int64
		if err := rows.Scan(&id, &title, &directory, &idleJSON, &idleSeq,
			&assistantJSON, &assistantSeq, &userSeq, &created, &updated); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		out = append(out, Session{
			Agent: "opencode", ID: id,
			Title:   nonEmpty(title, "(未命名会话)"),
			Cwd:     directory,
			Updated: formatMillis(updated),
			Created: formatMillis(created),
			Source:  path,
			Status:  opencodeStatus(idleJSON, idleSeq, assistantJSON, assistantSeq, userSeq, updated),
			kind:    "opencode",
			file:    path,
		})
	}
	return out, rows.Err()
}

// opencodeSessionTable picks the session table present in the database. Older
// builds only have `session`.
func opencodeSessionTable(db *sql.DB) string {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'session_v2'`).Scan(&name)
	if err == nil && name != "" {
		return "session_v2"
	}
	return "session"
}

type opencodeError struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Message string `json:"message"`
	Data    struct {
		Message string `json:"message"`
	} `json:"data"`
}

func (e opencodeError) detail() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Data.Message
}

func (e opencodeError) kind() string {
	if e.Type != "" {
		return e.Type
	}
	return e.Name
}

type opencodeAssistant struct {
	Finish string `json:"finish"`
	Error  opencodeError
	Time   struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// The two windows below decide when a silent turn stops looking like "still
// working". A turn with no `finish` was cut off mid-stream (the process died);
// a `tool-calls` turn may legitimately sit inside a long build or test run.
const (
	// unfinishedWindow 是「最后一轮没有 finish」还能算「正在跑」的时间；
	// 超过它就当作进程中途退出（OpenCode 崩溃时不会补 finish）。
	unfinishedWindow = 8 * time.Minute
	// toolWindow 是工具调用可能持续的上限：编译、测试、下载都算正常。
	toolWindow = 30 * time.Minute
)

// timeNow is a variable so tests can pin the clock; every status decision that
// depends on "how long ago" reads it instead of calling time.Now directly.
var timeNow = time.Now

// opencodeStatus derives the list-row status from the agent's own records.
//
// Two signals are combined, in this order:
//
//  1. `idle` is OpenCode's verdict on the turn it closed (succeeded, failed,
//     interrupted). It only describes that turn: once the user writes again,
//     the verdict is history and must not mark the session as stuck.
//  2. The last assistant turn: an `error` payload is a hard failure even when
//     no `idle` row was written (killed builds do this), an empty `finish`
//     means the reply never landed, and `tool-calls` means the agent is mid-run.
//
// The older `session` table has no `idle` rows at all, so the assistant turn is
// the only signal there; that is why it is consulted even when `idle` exists.
func opencodeStatus(idleJSON string, idleSeq int64, assistantJSON string, assistantSeq, userSeq int64, updated int64) Status {
	var idle struct {
		Outcome string `json:"outcome"`
		Time    struct {
			Created int64 `json:"created"`
		} `json:"time"`
	}
	_ = json.Unmarshal([]byte(idleJSON), &idle)

	var last opencodeAssistant
	_ = json.Unmarshal([]byte(assistantJSON), &last)

	// idle 是这条会话最新的事件时，它的结论才算数。
	idleIsLatest := idleSeq >= 0 && userSeq <= idleSeq && (assistantSeq < 0 || assistantSeq <= idleSeq)
	if idleIsLatest {
		switch idle.Outcome {
		case "failed":
			status := classify(last.Error.kind(), last.Error.detail())
			if status.Detail == "" {
				status.Detail = trimDetail(last.Error.detail())
			}
			status.At = formatMillis(idle.Time.Created)
			return status
		case "interrupted":
			return Status{
				State: "stuck", Code: "interrupted", Label: "被中断",
				At: formatMillis(idle.Time.Created),
			}
		case "succeeded":
			return done()
		}
	}

	// 用户在最后一轮之后又发了消息：要么正在跑，要么那一轮根本没起来。
	// 同一套时间窗用来区分两者，避免把「用户发完就放弃」的会话永久标成进行中。
	if userSeq > assistantSeq {
		if timeNow().Sub(time.UnixMilli(updated)) <= unfinishedWindow {
			return Status{State: "active", Code: "running", Label: "进行中"}
		}
		return Status{State: "stuck", Code: "interrupted", Label: "被中断", At: formatMillis(updated)}
	}

	lastAt := last.Time.Completed
	if lastAt == 0 {
		lastAt = last.Time.Created
	}
	if lastAt == 0 {
		lastAt = updated
	}

	if kind, detail := last.Error.kind(), last.Error.detail(); kind != "" || detail != "" {
		status := classify(kind, detail)
		status.At = formatMillis(lastAt)
		return status
	}
	if last.Finish == "error" {
		status := classify("", "")
		status.At = formatMillis(lastAt)
		return status
	}

	stale := timeNow().Sub(time.UnixMilli(lastAt))
	switch last.Finish {
	case "":
		if assistantJSON == "" {
			return done()
		}
		if stale > unfinishedWindow {
			return Status{State: "stuck", Code: "interrupted", Label: "被中断", At: formatMillis(lastAt)}
		}
		return Status{State: "active", Code: "running", Label: "进行中"}
	case "tool-calls":
		if stale > toolWindow {
			return Status{State: "stuck", Code: "interrupted", Label: "被中断", At: formatMillis(lastAt)}
		}
		return Status{State: "active", Code: "running", Label: "进行中"}
	}
	return done()
}

func (d *Detector) opencodeTranscript(session Session, full bool) (transcript, error) {
	db, err := openOpencodeDB(session.file)
	if err != nil {
		return transcript{}, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var data transcript
	data.Status = session.Status

	summary, summaryAt, boundarySeq, err := opencodeCompaction(ctx, db, session.ID)
	if err != nil {
		return transcript{}, err
	}
	data.Summary = summary
	data.SummaryAt = summaryAt

	if todos, err := opencodeTodos(ctx, db, session.ID); err == nil {
		data.Todos = todos
	}

	if boundarySeq > 0 {
		before, err := opencodeCountBefore(ctx, db, session.ID, boundarySeq)
		if err != nil {
			return transcript{}, err
		}
		data.BeforeCompaction = before
	}

	messages, err := opencodeMessages(ctx, db, session.ID, boundarySeq, full)
	if err != nil {
		return transcript{}, err
	}
	data.Messages = messages
	return data, nil
}

// opencodeCompaction returns the last completed compaction: its summary, when it
// ran, and the sequence number everything after it starts from.
func opencodeCompaction(ctx context.Context, db *sql.DB, sessionID string) (string, string, int64, error) {
	row := db.QueryRowContext(ctx, `
		SELECT seq, COALESCE(json_extract(data, '$.summary'), ''), COALESCE(time_created, 0)
		FROM session_message
		WHERE session_id = ?
		  AND type = 'compaction'
		  AND COALESCE(json_extract(data, '$.status'), 'completed') = 'completed'
		ORDER BY seq DESC
		LIMIT 1`, sessionID)

	var seq, created int64
	var summary string
	err := row.Scan(&seq, &summary, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, nil
	}
	if err != nil {
		return "", "", 0, err
	}
	return cleanSummary(summary), formatMillis(created), seq, nil
}

// opencodeCountBefore counts readable turns the last compaction replaced.
func opencodeCountBefore(ctx context.Context, db *sql.DB, sessionID string, boundarySeq int64) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM session_message
		WHERE session_id = ? AND seq <= ? AND type IN ('user', 'assistant')`,
		sessionID, boundarySeq).Scan(&count)
	return count, err
}

func opencodeTodos(ctx context.Context, db *sql.DB, sessionID string) ([]Todo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT content, status FROM todo
		WHERE session_id = ?
		ORDER BY position`, sessionID)
	if err != nil {
		// A store without a todo table simply has no tasks.
		return nil, nil
	}
	defer rows.Close()

	var out []Todo
	for rows.Next() {
		var content, status string
		if err := rows.Scan(&content, &status); err != nil {
			return nil, err
		}
		out = append(out, Todo{Text: cleanText(content), Status: status})
	}
	return out, rows.Err()
}

// opencodeMessages reads the conversation. boundarySeq clips to what followed
// the last compaction unless full is set.
func opencodeMessages(ctx context.Context, db *sql.DB, sessionID string, boundarySeq int64, full bool) ([]Turn, error) {
	query := `
		SELECT type, data FROM session_message
		WHERE session_id = ?`
	args := []any{sessionID}
	if boundarySeq > 0 && !full {
		query += ` AND seq > ?`
		args = append(args, boundarySeq)
	}
	query += ` ORDER BY seq ASC`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Turn
	for rows.Next() {
		var kind, raw string
		if err := rows.Scan(&kind, &raw); err != nil {
			return nil, err
		}
		switch kind {
		case "user":
			messages = appendTurn(messages, "用户", opencodeUserText(raw))
		case "assistant":
			messages = appendTurn(messages, "助手", opencodeAssistantText(raw))
		}
	}
	return messages, rows.Err()
}

func opencodeUserText(raw string) string {
	var row struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(raw), &row) == nil {
		return row.Text
	}
	return ""
}

type opencodePart struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	Ignored   bool   `json:"ignored"`
	Metadata  struct {
		Visibility string `json:"visibility"`
	} `json:"metadata"`
}

// opencodeAssistantText keeps the prose of one assistant turn and drops
// reasoning, tool calls and model-only reminder text.
func opencodeAssistantText(raw string) string {
	var row struct {
		Content []opencodePart `json:"content"`
	}
	if json.Unmarshal([]byte(raw), &row) != nil {
		return ""
	}
	values := make([]string, 0, len(row.Content))
	for _, part := range row.Content {
		if part.Type != "text" || part.Synthetic || part.Ignored {
			continue
		}
		if part.Metadata.Visibility == "model-only" {
			continue
		}
		if strings.TrimSpace(part.Text) != "" {
			values = append(values, part.Text)
		}
	}
	return strings.Join(values, "\n")
}
