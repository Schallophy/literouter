package stream

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Schallophy/literouter/internal/model"
)

func TestCanonicalJSON(t *testing.T) {
	got := CanonicalJSON(map[string]any{"b": 1.0, "a": []any{map[string]any{"d": 2.0, "c": 3.0}}})
	if got != `{"a":[{"c":3,"d":2}],"b":1}` {
		t.Fatalf("got %s", got)
	}
}

func TestResponsesInputItems(t *testing.T) {
	items := ResponsesInputItems("hi")
	if len(items) != 1 || items[0].(map[string]any)["role"] != "user" {
		t.Fatalf("got %v", items)
	}
	if ResponsesInputItems([]any{map[string]any{"role": "user"}}) == nil {
		t.Fatal("array input should pass through")
	}
	if ResponsesInputItems([]any{}) != nil || ResponsesInputItems(nil) != nil {
		t.Fatal("empty input must be nil")
	}
}

func TestPrefixHashesAndScope(t *testing.T) {
	items := []any{map[string]any{"role": "user", "content": "a"}, map[string]any{"role": "user", "content": "b"}}
	first := ItemsPrefixHashes("scope", items)
	second := ItemsPrefixHashes("scope", items)
	if len(first) != 2 || first[0] != second[0] || first[1] != second[1] || first[0] == first[1] {
		t.Fatalf("unstable hashes: %v %v", first, second)
	}
	other := ItemsPrefixHashes("other", items)
	if other[0] == first[0] {
		t.Fatal("scope must change hashes")
	}
	if ChainScope("caller", "provider", "model", nil) != ChainScope("caller", "provider", "model", nil) {
		t.Fatal("scope unstable")
	}
	if ChainScope("caller", "provider", "model", nil) == ChainScope("caller", "provider", "model", []any{1.0}) {
		t.Fatal("tools must change scope")
	}
}

func TestChainDelta(t *testing.T) {
	history := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
		map[string]any{"type": "function_call", "name": "ls", "call_id": "c1"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "a.txt"},
		map[string]any{"role": "user", "content": "next"},
	}
	delta := ChainDelta(history, 1)
	if len(delta) != 2 || delta[0].(map[string]any)["type"] != "function_call_output" || delta[1].(map[string]any)["content"] != "next" {
		t.Fatalf("got %v", delta)
	}
	if got := ChainDelta(history, 3); len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if got := ChainDelta([]any{map[string]any{"role": "user", "content": "a"}, map[string]any{"role": "user", "content": "b"}}, 1); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if ChainDelta(history, len(history)) != nil {
		t.Fatal("fully stored input must not chain")
	}
	if ChainDelta([]any{map[string]any{"role": "user", "content": "a"}, map[string]any{"role": "assistant", "content": "b"}}, 1) != nil {
		t.Fatal("only stored output must not chain")
	}
}

func TestResponseChainStoreTTLAndLRU(t *testing.T) {
	store := NewResponseChainStore()
	store.Remember("a", "id-a", 1000, 2, 0)
	store.Remember("b", "id-b", 1000, 2, 0)
	if got := store.Lookup("a", 500); got != "id-a" {
		t.Fatalf("got %q", got)
	}
	store.Remember("c", "id-c", 1000, 2, 500)
	if store.Size() != 2 {
		t.Fatalf("size %d", store.Size())
	}
	if got := store.Lookup("b", 600); got != "" {
		t.Fatalf("b should be evicted, got %q", got)
	}
	if got := store.Lookup("a", 600); got != "id-a" {
		t.Fatalf("a should survive, got %q", got)
	}
	if store.Lookup("a", 2000) != "" || store.Lookup("c", 2000) != "" || store.Size() != 0 {
		t.Fatalf("entries should expire, size %d", store.Size())
	}
}

type pieceReader struct {
	pieces []string
	index  int
}

func (r *pieceReader) Read(p []byte) (int, error) {
	if r.index >= len(r.pieces) {
		return 0, io.EOF
	}
	piece := r.pieces[r.index]
	r.index++
	copy(p, piece)
	return len(piece), nil
}

func (r *pieceReader) Close() error { return nil }

func readAll(t *testing.T, body bodySource) string {
	t.Helper()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

func TestCaptureResponseIDSSE(t *testing.T) {
	events := []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_123\"}}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
		"data: [DONE]\n\n",
	}
	body := CaptureResponseID(&pieceReader{pieces: events}, func(id string) {
		if id != "resp_123" {
			t.Fatalf("id %q", id)
		}
	})
	if got := readAll(t, body); got != strings.Join(events, "") {
		t.Fatalf("bytes changed: %q", got)
	}
}

func TestCaptureResponseIDSplit(t *testing.T) {
	whole := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_split\"}}\n\n"
	parts := []string{whole[:20], whole[20:55], whole[55:]}
	captured := ""
	body := CaptureResponseID(&pieceReader{pieces: parts}, func(id string) { captured = id })
	if got := readAll(t, body); got != whole {
		t.Fatalf("bytes changed: %q", got)
	}
	if captured != "resp_split" {
		t.Fatalf("captured %q", captured)
	}
}

func TestCaptureResponseIDJSON(t *testing.T) {
	captured := ""
	body := CaptureResponseID(&pieceReader{pieces: []string{`{"id":"resp_json","output":[]}`}}, func(id string) { captured = id })
	if got := readAll(t, body); got != `{"id":"resp_json","output":[]}` {
		t.Fatalf("bytes changed: %q", got)
	}
	if captured != "resp_json" {
		t.Fatalf("captured %q", captured)
	}
}

func TestExtractOutputText(t *testing.T) {
	chat := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "hello"}}}}
	if got := ExtractOutputText(model.ApiChat, chat); got == nil || *got != "hello" {
		t.Fatalf("chat: %v", got)
	}
	completions := map[string]any{"choices": []any{map[string]any{"text": "a"}, map[string]any{"text": "b"}}}
	if got := ExtractOutputText(model.ApiCompletions, completions); got == nil || *got != "ab" {
		t.Fatalf("completions: %v", got)
	}
	responses := map[string]any{"output": []any{
		map[string]any{"content": []any{map[string]any{"text": "x"}}},
		map[string]any{"text": "y"},
	}}
	if got := ExtractOutputText(model.ApiResponses, responses); got == nil || *got != "x\ny" {
		t.Fatalf("responses: %v", got)
	}
	embeddings := map[string]any{"data": []any{map[string]any{"embedding": []any{1.0, 2.0, 3.0}}}}
	if got := ExtractOutputText(model.ApiEmbeddings, embeddings); got == nil || *got != "1 条向量 × 3 维" {
		t.Fatalf("embeddings: %v", got)
	}
	if ExtractOutputText(model.ApiChat, nil) != nil {
		t.Fatal("nil data")
	}
}

func TestExtractToolCalls(t *testing.T) {
	chat := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{
		map[string]any{"function": map[string]any{"name": "ls", "arguments": `{"path":"."}`}},
	}}}}}
	calls := ExtractToolCalls(model.ApiChat, chat)
	if len(calls) != 1 || calls[0].Name != "ls" || calls[0].Arguments != `{"path":"."}` {
		t.Fatalf("chat: %+v", calls)
	}
	responses := map[string]any{"output": []any{
		map[string]any{"type": "function_call", "name": "lookup", "arguments": "{}"},
		map[string]any{"type": "message", "name": "ignored"},
	}}
	calls = ExtractToolCalls(model.ApiResponses, responses)
	if len(calls) != 1 || calls[0].Name != "lookup" {
		t.Fatalf("responses: %+v", calls)
	}
}

func TestSSEOutputChatDeltas(t *testing.T) {
	extractor := NewSSEOutputExtractor(model.ApiChat)
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}]}\n\n"
	var text strings.Builder
	for index := 0; index < len(stream); index += 7 {
		end := index + 7
		if end > len(stream) {
			end = len(stream)
		}
		text.WriteString(extractor.Push(stream[index:end]))
	}
	text.WriteString(extractor.Flush())
	if text.String() != "Hello" {
		t.Fatalf("got %q", text.String())
	}
	if extractor.FinishReason() == nil || *extractor.FinishReason() != "stop" {
		t.Fatalf("finish %v", extractor.FinishReason())
	}
}

func TestSSEOutputToolCalls(t *testing.T) {
	extractor := NewSSEOutputExtractor(model.ApiChat)
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"ls\",\"arguments\":\"{\\\"pa\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"th\\\":\\\".\\\"}\"}}]}}]}\n\n"
	extractor.Push(stream)
	calls := extractor.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "ls" || calls[0].Arguments != `{"path":"."}` {
		t.Fatalf("got %+v", calls)
	}
}

func TestSSEOutputResponsesDeltas(t *testing.T) {
	extractor := NewSSEOutputExtractor(model.ApiResponses)
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\" there\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output_text\":\"ignored\"}}\n\n"
	if got := extractor.Push(stream); got != "Hi there" {
		t.Fatalf("got %q", got)
	}
	if extractor.FinishReason() == nil || *extractor.FinishReason() != "completed" {
		t.Fatalf("finish %v", extractor.FinishReason())
	}
}

func TestSSEOutputResponsesCompletedFallback(t *testing.T) {
	extractor := NewSSEOutputExtractor(model.ApiResponses)
	stream := "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"content\":[{\"text\":\"full text\"}]}]}}\n\n"
	if got := extractor.Push(stream); got != "full text" {
		t.Fatalf("got %q", got)
	}
}

func TestSSEOutputResponsesToolCalls(t *testing.T) {
	extractor := NewSSEOutputExtractor(model.ApiResponses)
	stream := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item_1\",\"name\":\"lookup\"},\"output_index\":0}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_1\",\"delta\":\"{\\\"q\\\":\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item_1\",\"delta\":\"1}\"}\n\n"
	extractor.Push(stream)
	calls := extractor.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "lookup" || calls[0].Arguments != `{"q":1}` {
		t.Fatalf("got %+v", calls)
	}
}

func TestLiveOutputLifecycle(t *testing.T) {
	log := NewLiveOutputLog(2, 10, 20)
	var events []LiveOutputEvent
	unsubscribe := log.Subscribe(func(event LiveOutputEvent) { events = append(events, event) })
	defer unsubscribe()

	handle := log.Start(LiveOutputStart{API: model.ApiChat, Model: "m", Caller: "c", Requested: "auto", Stream: true})
	handle.Push("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
	handle.Finish(LiveOutputFinish{})

	recent := log.Recent(10)
	if len(recent) != 1 || recent[0].Text != "hello" || recent[0].Status != LiveOK {
		t.Fatalf("got %+v", recent)
	}
	if len(events) < 2 || events[0].Type != "start" || events[len(events)-1].Type != "done" {
		t.Fatalf("events %+v", events)
	}
	log.Clear()
	if len(log.Recent(10)) != 0 {
		t.Fatal("clear failed")
	}
}

func TestLiveOutputTruncation(t *testing.T) {
	log := NewLiveOutputLog(5, 8, 10)
	handle := log.Start(LiveOutputStart{API: model.ApiCompletions, Model: "m"})
	handle.Push("data: {\"choices\":[{\"text\":\"123456789012345\"}]}\n\n")
	handle.Finish(LiveOutputFinish{})
	recent := log.Recent(1)
	if len(recent) != 1 {
		t.Fatal("missing session")
	}
	if !recent[0].Truncated || !strings.Contains(recent[0].Text, "已截断") {
		t.Fatalf("not truncated: %+v", recent[0])
	}
	if recent[0].RawLength <= 10 || !recent[0].RawTruncated {
		t.Fatalf("raw should be truncated: %d %v", recent[0].RawLength, recent[0].RawTruncated)
	}
}

func TestLiveOutputToolTrim(t *testing.T) {
	calls := make([]OutputToolCall, 10)
	for index := range calls {
		calls[index] = OutputToolCall{Name: "f", Arguments: strings.Repeat("x", 5000)}
	}
	log := NewLiveOutputLog(5, 1000, 1000)
	handle := log.Start(LiveOutputStart{API: model.ApiChat, Model: "m"})
	handle.Finish(LiveOutputFinish{ToolCalls: calls})
	recent := log.Recent(1)
	if len(recent[0].ToolCalls) != 8 {
		t.Fatalf("tool calls %d", len(recent[0].ToolCalls))
	}
	if !strings.HasSuffix(recent[0].ToolCalls[0].Arguments, "…") || len([]rune(recent[0].ToolCalls[0].Arguments)) != 4001 {
		t.Fatalf("args not trimmed")
	}
}

func TestLiveOutputRingTrim(t *testing.T) {
	log := NewLiveOutputLog(1, 1000, 1000)
	first := log.Start(LiveOutputStart{API: model.ApiChat, Model: "a"})
	first.Finish(LiveOutputFinish{})
	second := log.Start(LiveOutputStart{API: model.ApiChat, Model: "b"})
	second.Finish(LiveOutputFinish{})
	recent := log.Recent(10)
	if len(recent) != 1 || recent[0].Model != "b" {
		t.Fatalf("got %+v", recent)
	}
}

func TestEventLogRingAndFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "events.jsonl")
	log := NewEventLog(file, 3)
	for index := 0; index < 5; index++ {
		log.Push(EventInput{Kind: EventConfig, Level: LevelInfo, Message: "m"})
	}
	recent := log.Recent(10, "")
	if len(recent) != 3 || recent[0].ID != 5 {
		t.Fatalf("got %+v", recent)
	}
	filtered := log.Recent(10, EventDead)
	if len(filtered) != 0 {
		t.Fatalf("got %+v", filtered)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Count(string(data), "\n") != 5 {
		t.Fatalf("expected 5 lines, got %q", string(data))
	}
}
