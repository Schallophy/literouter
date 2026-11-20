package sessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestZcodeDBSessionsAndTranscript(t *testing.T) {
	path := newZcodeDB(t)
	items, err := zcodeDBSessions(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("sessions = %d, want 2: %+v", len(items), items)
	}
	if items[0].ID != "sess_new" || items[0].Title != "新会话" || items[0].Cwd != `C:\new` {
		t.Fatalf("first session = %+v", items[0])
	}
	if items[0].Updated == "" || items[0].kind != "zcode-db" {
		t.Fatalf("session metadata missing: %+v", items[0])
	}

	data, err := zcodeDBTranscript(path, "sess_new", false)
	if err != nil {
		t.Fatal(err)
	}
	if data.Summary != "" {
		t.Fatalf("unexpected summary: %q", data.Summary)
	}
	messages := data.Messages
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3: %+v", len(messages), messages)
	}
	want := []string{"你好", "在。", "真正的问题"}
	for index, text := range want {
		if messages[index].Text != text {
			t.Fatalf("messages[%d] = %q, want %q", index, messages[index].Text, text)
		}
	}
	for _, item := range messages {
		for _, noise := range []string{"think", "noise", "reminder", "hidden", "tool output"} {
			if strings.Contains(item.Text, noise) {
				t.Fatalf("unexpected %q leaked into %q", noise, item.Text)
			}
		}
	}
}

func TestZcodeDBTranscriptUsesCompactionSummary(t *testing.T) {
	path := newZcodeDB(t)
	data, err := zcodeDBTranscript(path, "sess_old", false)
	if err != nil {
		t.Fatal(err)
	}
	if data.Summary != "旧会话压缩摘要" {
		t.Fatalf("summary = %q", data.Summary)
	}
	if data.SummaryAt == "" {
		t.Fatal("summary timestamp is missing")
	}
	if len(data.Messages) != 2 {
		t.Fatalf("messages = %d, want 2: %+v", len(data.Messages), data.Messages)
	}
	if data.Messages[0].Text != "压缩后的问题" || data.Messages[1].Text != "压缩后的回答" {
		t.Fatalf("messages = %+v", data.Messages)
	}

	text := BuildHandoffTranscript(
		Session{Agent: "zcode", ID: "sess_old", Title: "旧会话"},
		data,
	)
	for _, want := range []string{"上次压缩摘要", "旧会话压缩摘要", "压缩后的问题"} {
		if !strings.Contains(text, want) {
			t.Fatalf("handoff is missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "旧回答") {
		t.Fatal("pre-compaction history leaked into the handoff")
	}
}

func TestZcodeDBTranscriptFullKeepsHistory(t *testing.T) {
	path := newZcodeDB(t)
	data, err := zcodeDBTranscript(path, "sess_old", true)
	if err != nil {
		t.Fatal(err)
	}
	if data.Summary != "旧会话压缩摘要" {
		t.Fatalf("summary = %q", data.Summary)
	}
	if len(data.Messages) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(data.Messages), data.Messages)
	}
	want := []string{"旧问题", "旧回答", "压缩后的问题", "压缩后的回答"}
	for index, text := range want {
		if data.Messages[index].Text != text {
			t.Fatalf("messages[%d] = %q, want %q", index, data.Messages[index].Text, text)
		}
	}
}

func TestDetectorReadsZcodeDatabase(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".zcode", "cli", "db")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	newZcodeDBAt(t, filepath.Join(dir, "db.sqlite"))

	detector := &Detector{home: home}
	result := detector.List()
	if len(result.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2: %+v", len(result.Sessions), result.Sessions)
	}
	for _, agent := range result.Agents {
		if agent.ID == "zcode" && !agent.Present {
			t.Fatal("zcode should be detected from its database")
		}
	}

	handoff, err := detector.Handoff("zcode", "sess_new")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"新会话", "在。", "真正的问题"} {
		if !strings.Contains(handoff.Text, want) {
			t.Fatalf("handoff is missing %q: %s", want, handoff.Text)
		}
	}
	if strings.Contains(handoff.Text, "已省略") {
		t.Fatalf("handoff should keep every post-compaction message: %s", handoff.Text)
	}

	detail, err := detector.Detail("zcode", "sess_old")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Title != "旧会话" || detail.Summary == "" || len(detail.Messages) != 4 {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.Chars == 0 {
		t.Fatal("detail character count is missing")
	}
}

func newZcodeDB(t *testing.T) string {
	t.Helper()
	return newZcodeDBAt(t, filepath.Join(t.TempDir(), "db.sqlite"))
}

func newZcodeDBAt(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	statements := []string{
		`CREATE TABLE session (
			id TEXT PRIMARY KEY, title TEXT, directory TEXT,
			time_created INTEGER, time_updated INTEGER, time_deleted INTEGER)`,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER,
			time_updated INTEGER, data TEXT, sequence INTEGER)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT,
			time_created INTEGER, time_updated INTEGER, data TEXT, sequence INTEGER)`,
		`INSERT INTO session VALUES ('sess_old', '旧会话', 'C:\old', 1000, 2000, NULL)`,
		`INSERT INTO session VALUES ('sess_new', '新会话', 'C:\new', 3000, 4000, NULL)`,
		`INSERT INTO session VALUES ('sess_gone', '已删除', 'C:\gone', 5000, 6000, 1)`,
		`INSERT INTO message VALUES ('o1', 'sess_old', 1000, 1000, '{"role":"user"}', 0)`,
		`INSERT INTO part VALUES ('op1', 'o1', 'sess_old', 1000, 1000, '{"type":"text","text":"旧问题"}', 0)`,
		`INSERT INTO message VALUES ('o2', 'sess_old', 1100, 1100, '{"role":"assistant"}', 1)`,
		`INSERT INTO part VALUES ('op2', 'o2', 'sess_old', 1100, 1100, '{"type":"text","text":"旧回答"}', 0)`,
		`INSERT INTO message VALUES ('o3', 'sess_old', 1200, 1200, '{"role":"user","semantics":{"origin":"agent_runtime","kind":"compact_summary","uiVisibility":"hidden","providerVisibility":"visible","transcriptVisibility":"hidden"},"summary":{"title":"Compact summary","body":"旧会话压缩摘要"}}', 2)`,
		`INSERT INTO part VALUES ('op3', 'o3', 'sess_old', 1200, 1200, '{"type":"text","text":"旧会话压缩摘要","synthetic":true}', 0)`,
		`INSERT INTO part VALUES ('op4', 'o3', 'sess_old', 1200, 1200, '{"type":"compaction","auto":true,"trigger":"auto","compactReason":"context_limit","summaryMessageId":"o3","tail_start_id":"o2","compactBoundary":{"lastSummarizedMessageId":"o2","summaryMessageIds":["o3"]}}', 1)`,
		`INSERT INTO message VALUES ('o4', 'sess_old', 1300, 1300, '{"role":"user"}', 3)`,
		`INSERT INTO part VALUES ('op5', 'o4', 'sess_old', 1300, 1300, '{"type":"text","text":"压缩后的问题"}', 0)`,
		`INSERT INTO message VALUES ('o5', 'sess_old', 1400, 1400, '{"role":"assistant"}', 4)`,
		`INSERT INTO part VALUES ('op6', 'o5', 'sess_old', 1400, 1400, '{"type":"text","text":"压缩后的回答"}', 0)`,
		`INSERT INTO message VALUES ('m1', 'sess_new', 3000, 3000, '{"role":"user"}', 0)`,
		`INSERT INTO message VALUES ('m2', 'sess_new', 3100, 3100, '{"role":"assistant"}', 1)`,
		`INSERT INTO message VALUES ('m3', 'sess_new', 3200, 3200, '{"role":"user"}', 2)`,
		`INSERT INTO part VALUES ('p1', 'm1', 'sess_new', 3000, 3000, '{"type":"text","text":"你好"}', 0)`,
		`INSERT INTO part VALUES ('p2', 'm1', 'sess_new', 3000, 3000, '{"type":"text","text":"noise","synthetic":true}', 1)`,
		`INSERT INTO part VALUES ('p3', 'm2', 'sess_new', 3100, 3100, '{"type":"reasoning","text":"think"}', 0)`,
		`INSERT INTO part VALUES ('p4', 'm2', 'sess_new', 3100, 3100, '{"type":"text","text":"在。"}', 1)`,
		`INSERT INTO part VALUES ('p5', 'm2', 'sess_new', 3100, 3100, '{"type":"tool","text":"tool output"}', 2)`,
		`INSERT INTO part VALUES ('p6', 'm2', 'sess_new', 3100, 3100, '{"type":"text","text":"reminder","metadata":{"visibility":"model-only"}}', 3)`,
		`INSERT INTO part VALUES ('p8', 'm3', 'sess_new', 3200, 3200, '{"type":"text","text":"<system-reminder>ignore</system-reminder>真正的问题"}', 0)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}
