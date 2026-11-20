package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// zcodeDBPath returns the SQLite database that ZCode writes its full history to.
func zcodeDBPath(home string) string {
	return joinHome(home, ".zcode", "cli", "db", "db.sqlite")
}

// openZcodeDB opens the ZCode database read-only so the detector never blocks
// or mutates a running ZCode.
func openZcodeDB(path string) (*sql.DB, error) {
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

func zcodeDBSessions(path string) ([]Session, error) {
	db, err := openZcodeDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(title, ''), COALESCE(directory, ''),
		       COALESCE(time_created, 0), COALESCE(time_updated, 0)
		FROM session
		WHERE COALESCE(time_deleted, 0) = 0
		ORDER BY time_updated DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var id, title, directory string
		var created, updated int64
		if err := rows.Scan(&id, &title, &directory, &created, &updated); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		out = append(out, Session{
			Agent: "zcode", ID: id, Title: nonEmpty(title, id), Cwd: directory,
			Updated: formatMillis(updated), Created: formatMillis(created),
			Source: path, kind: "zcode-db", file: path,
		})
	}
	return out, rows.Err()
}

func zcodeDBTranscript(path, sessionID string, full bool) (transcript, error) {
	db, err := openZcodeDB(path)
	if err != nil {
		return transcript{}, err
	}
	defer db.Close()

	_, compactSeq, compactAt, summary, err := zcodeDBCompaction(db, sessionID)
	if err != nil {
		return transcript{}, err
	}
	boundarySeq := int64(-1)
	if !full {
		boundarySeq = compactSeq
	}
	var data transcript
	if summary != "" {
		data.Summary = summary
		data.SummaryAt = formatMillis(compactAt)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.data, p.data
		FROM message m
		JOIN part p ON p.message_id = m.id
		WHERE m.session_id = ?
		  AND m.id IN (
		    SELECT id FROM message
		    WHERE session_id = ?
		      AND COALESCE(sequence, 0) > ?)
		ORDER BY m.sequence ASC, m.time_created ASC, p.sequence ASC`,
		sessionID, sessionID, boundarySeq)
	if err != nil {
		return transcript{}, err
	}
	defer rows.Close()

	var messages []message
	var currentID, currentRole string
	var parts []string
	flush := func() {
		if currentID == "" || len(parts) == 0 {
			parts = nil
			return
		}
		role := "助手"
		if currentRole == "user" {
			role = "用户"
		}
		messages = appendTurn(messages, role, strings.Join(parts, "\n"), full)
		parts = nil
	}

	for rows.Next() {
		var messageID, messageData, partData string
		if err := rows.Scan(&messageID, &messageData, &partData); err != nil {
			return transcript{}, err
		}
		if messageID != currentID {
			flush()
			currentID = messageID
			currentRole = zcodeDBRole(messageData)
		}
		if currentRole != "user" && currentRole != "assistant" {
			continue
		}
		var part zcodeDBPart
		if json.Unmarshal([]byte(partData), &part) != nil {
			continue
		}
		// 只保留正文：跳过 reasoning、工具调用和模型内部的提醒文本。
		if part.Type != "text" || part.Synthetic || part.Metadata.Visibility == "model-only" {
			continue
		}
		if text := cleanText(part.Text); text != "" {
			parts = append(parts, text)
		}
	}
	if err := rows.Err(); err != nil {
		return transcript{}, err
	}
	flush()
	data.Messages = messages
	return data, nil
}

// zcodeDBCompaction locates the last compaction ZCode wrote. ZCode marks the
// generated context as a hidden user message with
// semantics.kind = "compact_summary"; its text part holds the summary and a
// sibling "compaction" part holds the boundary and token counts.
func zcodeDBCompaction(db *sql.DB, sessionID string) (id string, sequence, created int64, summary string, err error) {
	row := db.QueryRow(`
		SELECT id, COALESCE(sequence, 0), COALESCE(time_created, 0),
		       COALESCE(json_extract(data, '$.summary.body'), '')
		FROM message
		WHERE session_id = ?
		  AND (json_extract(data, '$.semantics.kind') = 'compact_summary'
		       OR json_extract(data, '$.summary.body') IS NOT NULL)
		ORDER BY sequence DESC, time_created DESC
		LIMIT 1`, sessionID)
	err = row.Scan(&id, &sequence, &created, &summary)
	if err == sql.ErrNoRows {
		return "", -1, 0, "", nil
	}
	if err != nil {
		return "", -1, 0, "", err
	}
	if summary = cleanSummary(summary); summary != "" {
		return id, sequence, created, summary, nil
	}

	// Some rows keep the text only in the message parts.
	rows, err := db.Query(`
		SELECT data FROM part
		WHERE message_id = ? AND json_extract(data, '$.type') = 'text'
		ORDER BY sequence`, id)
	if err != nil {
		return "", -1, 0, "", err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return "", -1, 0, "", err
		}
		var part zcodeDBPart
		if json.Unmarshal([]byte(data), &part) == nil {
			if text := cleanSummary(part.Text); text != "" {
				parts = append(parts, text)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", -1, 0, "", err
	}
	return id, sequence, created, strings.Join(parts, "\n"), nil
}

type zcodeDBMessage struct {
	Role string `json:"role"`
}

type zcodeDBPart struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	Metadata  struct {
		Visibility string `json:"visibility"`
	} `json:"metadata"`
}

func zcodeDBRole(data string) string {
	var message zcodeDBMessage
	if json.Unmarshal([]byte(data), &message) != nil {
		return ""
	}
	return message.Role
}
