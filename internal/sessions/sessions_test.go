package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMessagesKeepFullText(t *testing.T) {
	long := strings.Repeat("长", 5000)
	messages := appendMessage(nil, "用户", long)
	if len(messages) != 1 || messages[0].Text != long {
		t.Fatalf("message text was capped: %d", utf8.RuneCountInString(messages[0].Text))
	}
}

func TestBuildHandoffKeepsEveryPostCompactionMessage(t *testing.T) {
	var messages []message
	for index := 0; index < 400; index++ {
		messages = append(messages, message{
			Role: "用户",
			Text: fmt.Sprintf("第 %d 条消息：%s", index, strings.Repeat("内容", 50)),
		})
	}
	session := Session{
		Agent: "qoder", ID: "demo", Title: "标题",
		Cwd: `C:\demo`, Updated: "2026-11-20T10:00:00+08:00",
	}
	text := BuildHandoff(session, messages)
	for _, index := range []int{0, 199, 399} {
		if !strings.Contains(text, fmt.Sprintf("第 %d 条消息", index)) {
			t.Fatalf("message %d is missing from the handoff", index)
		}
	}
	if strings.Contains(text, "已省略") {
		t.Fatal("handoff should not drop messages")
	}
}

func TestBuildHandoffEmpty(t *testing.T) {
	text := BuildHandoff(Session{Agent: "qoder", ID: "demo"}, nil)
	if !strings.Contains(text, "没有可用的对话文本") {
		t.Fatalf("missing empty note: %s", text)
	}
}

func TestBuildHandoffPrefersSummary(t *testing.T) {
	session := Session{Agent: "qoder", ID: "demo", Title: "标题"}
	messages := []message{
		{Role: "用户", Text: "压缩后的第一条"},
		{Role: "助手", Text: "压缩后的最后一条"},
	}

	long := transcript{
		Summary:   strings.Repeat("压缩摘要内容", 600),
		SummaryAt: "2026-11-20T10:00:00+08:00",
		Messages:  messages,
	}
	text := BuildHandoffTranscript(session, long)
	if !strings.Contains(text, "## 上次压缩摘要") {
		t.Fatal("compaction summary heading is missing")
	}
	if strings.Count(text, "压缩摘要内容") != 600 {
		t.Fatal("compaction summary should not be clipped")
	}
	if !strings.Contains(text, "压缩后的第一条") || !strings.Contains(text, "压缩后的最后一条") {
		t.Fatal("post-compaction messages are missing")
	}
	if strings.Contains(text, "已省略") {
		t.Fatal("handoff should not drop messages")
	}

	short := transcript{Summary: "短摘要", SummaryAt: "2026-11-20T10:00:00+08:00", Messages: messages}
	text = BuildHandoffTranscript(session, short)
	if !strings.Contains(text, "短摘要") || !strings.Contains(text, "压缩后的对话") {
		t.Fatal("summary or post-compaction section is missing")
	}
	if !strings.Contains(text, "压缩后的最后一条") {
		t.Fatal("newest post-compaction message is missing")
	}
}

func TestQoderHead(t *testing.T) {
	title, cwd := qoderHead(filepath.Join("testdata", "qoder.jsonl"))
	if title != "请把 serm 改成可以非交互式操作" {
		t.Fatalf("title = %q", title)
	}
	if cwd != `C:\Users\demo\projects` {
		t.Fatalf("cwd = %q", cwd)
	}
}

func TestQoderTranscript(t *testing.T) {
	data, err := qoderTranscript(filepath.Join("testdata", "qoder.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	if data.Summary != "Summary of the conversation so far:\n已确认 serm 支持非交互式操作。" {
		t.Fatalf("summary = %q", data.Summary)
	}
	if len(data.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(data.Messages))
	}
	if data.Messages[0].Role != "用户" || data.Messages[0].Text != "加一个 --yes 参数" {
		t.Fatalf("first message = %+v", data.Messages[0])
	}
	for _, item := range data.Messages {
		if strings.Contains(item.Text, "internal reasoning") {
			t.Fatal("reasoning text leaked into the transcript")
		}
	}
}

func TestWorkbuddyHeadAndTranscript(t *testing.T) {
	file := filepath.Join("testdata", "workbuddy.jsonl")
	if title := workbuddyTitle(file); title != "询问 RustDesk 是否支持手柄" {
		t.Fatalf("title = %q", title)
	}
	if cwd := workbuddyCwd(file); cwd != `c:\Users\demo\projects` {
		t.Fatalf("cwd = %q", cwd)
	}
	data, err := workbuddyTranscript(file, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data.Summary, "已确认 RustDesk 支持部分手柄") {
		t.Fatalf("summary = %q", data.Summary)
	}
	if data.SummaryAt == "" {
		t.Fatal("summary timestamp is missing")
	}
	if len(data.Messages) != 1 || data.Messages[0].Text != "还需要配置映射。" {
		t.Fatalf("messages = %+v", data.Messages)
	}
}

func TestDecodeOpencodeExport(t *testing.T) {
	file, err := os.Open(filepath.Join("testdata", "opencode-export.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := decodeOpencodeExport(file, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data.Summary, "补上 GitHub Actions CI") {
		t.Fatalf("summary = %q", data.Summary)
	}
	if data.SummaryAt == "" {
		t.Fatal("summary timestamp is missing")
	}
	if len(data.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(data.Messages))
	}
	if data.Messages[0].Text != "再检查一下缓存" {
		t.Fatalf("first post-compaction message = %q", data.Messages[0].Text)
	}
	for _, item := range data.Messages {
		if strings.Contains(item.Text, "想一想") {
			t.Fatal("reasoning text leaked into the transcript")
		}
	}
}

func TestTranscriptFullKeepsPreCompactionHistory(t *testing.T) {
	qoder, err := qoderTranscript(filepath.Join("testdata", "qoder.jsonl"), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(qoder.Messages) != 4 {
		t.Fatalf("qoder messages = %d, want 4", len(qoder.Messages))
	}
	if qoder.Summary == "" {
		t.Fatal("qoder summary is missing")
	}

	workbuddy, err := workbuddyTranscript(filepath.Join("testdata", "workbuddy.jsonl"), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(workbuddy.Messages) != 3 {
		t.Fatalf("workbuddy messages = %d, want 3", len(workbuddy.Messages))
	}
	if workbuddy.Messages[0].Text != "询问 RustDesk 是否支持手柄" {
		t.Fatalf("workbuddy first message = %q", workbuddy.Messages[0].Text)
	}

	file, err := os.Open(filepath.Join("testdata", "opencode-export.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	opencode, err := decodeOpencodeExport(file, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(opencode.Messages) != 6 {
		t.Fatalf("opencode messages = %d, want 6", len(opencode.Messages))
	}
	if opencode.Messages[0].Text != "帮我修一下构建脚本" {
		t.Fatalf("opencode first message = %q", opencode.Messages[0].Text)
	}
}

func TestZcodeTranscript(t *testing.T) {
	file := filepath.Join("testdata", "zcode.jsonl")
	record, err := lastZcodeRecord(file)
	if err != nil {
		t.Fatal(err)
	}
	title, cwd := zcodeSummary(record)
	if title != "酷狗概念版签到接口仓库排查" {
		t.Fatalf("title = %q", title)
	}
	if cwd != `C:\Users\demo\projects` {
		t.Fatalf("cwd = %q", cwd)
	}
	messages, err := zcodeTranscript(file, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(messages))
	}
}

// TestLiveDetector scans this machine; run with LITEROUTER_LIVE_TEST=1 to see
// what the detector actually finds.
func TestLiveDetector(t *testing.T) {
	if os.Getenv("LITEROUTER_LIVE_TEST") != "1" {
		t.Skip("set LITEROUTER_LIVE_TEST=1 to scan this machine")
	}
	detector := NewDetector()
	result := detector.List()
	for _, agent := range result.Agents {
		t.Logf("agent %s present=%v store=%s", agent.Label, agent.Present, agent.Store)
	}
	for _, session := range result.Sessions {
		t.Logf("%-10s %-40s %s  %s", session.Agent, session.Title, session.Updated, session.Cwd)
	}
	if len(result.Sessions) == 0 {
		t.Fatal("no sessions found")
	}
	for _, message := range result.Errors {
		t.Logf("error: %s", message)
	}
}

func TestDetectorListSorts(t *testing.T) {
	home := t.TempDir()
	qoderDir := filepath.Join(home, ".qoder", "projects", "demo")
	workDir := filepath.Join(home, ".workbuddy", "projects", "demo")
	zcodeDir := filepath.Join(home, ".zcode", "cli", "rollout")
	for _, dir := range []string{qoderDir, workDir, zcodeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	qoderFile := filepath.Join(qoderDir, "qoder-demo.jsonl")
	workFile := filepath.Join(workDir, "wb-demo.jsonl")
	zcodeFile := filepath.Join(zcodeDir, "model-io-sess_zcode-demo.jsonl")
	copyFile(t, filepath.Join("testdata", "qoder.jsonl"), qoderFile)
	copyFile(t, filepath.Join("testdata", "workbuddy.jsonl"), workFile)
	copyFile(t, filepath.Join("testdata", "zcode.jsonl"), zcodeFile)

	setModTime(t, qoderFile, "2026-11-17T10:00:00Z")
	setModTime(t, workFile, "2026-11-19T10:00:00Z")

	detector := &Detector{home: home}
	result := detector.List()
	if len(result.Sessions) != 3 {
		t.Fatalf("sessions = %d, want 3: %+v", len(result.Sessions), result.Sessions)
	}
	want := []string{"zcode", "workbuddy", "qoder"}
	for index, agent := range want {
		if result.Sessions[index].Agent != agent {
			t.Fatalf("order[%d] = %s, want %s", index, result.Sessions[index].Agent, agent)
		}
	}
	for _, agent := range result.Agents {
		switch agent.ID {
		case "qoder", "workbuddy", "zcode":
			if !agent.Present {
				t.Fatalf("agent %s should be detected from the store dir", agent.ID)
			}
		case "opencode":
			if agent.Present {
				t.Fatal("opencode should not be detected in an empty home")
			}
		}
	}
}

func TestDetectorHandoffMissing(t *testing.T) {
	detector := &Detector{home: t.TempDir()}
	if _, err := detector.Handoff("qoder", "missing"); err == nil {
		t.Fatal("expected an error for an unknown session")
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func setModTime(t *testing.T, file, value string) {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, at, at); err != nil {
		t.Fatal(err)
	}
}
