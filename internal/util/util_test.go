package util

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Schallophy/literouter/internal/model"
)

func TestLabelFor(t *testing.T) {
	cases := map[string]string{
		"sk-opencode": "opencode",
		"sk-cline":    "cline",
		"local":       "local",
	}
	for token, want := range cases {
		if got := LabelFor(token); got != want {
			t.Fatalf("%q -> %q", token, got)
		}
	}
	long := "sk-1234567890abcdefghijklmnopqrstuvwxyz"
	label := LabelFor(long)
	if !strings.Contains(label, "…") || strings.Contains(label, long) {
		t.Fatalf("long token not masked: %q", label)
	}
}

func TestIdentifyCaller(t *testing.T) {
	first := IdentifyCallerValue("Bearer sk-opencode")
	second := IdentifyCallerValue("Bearer sk-opencode")
	other := IdentifyCallerValue("Bearer sk-cline")
	if len(first.ID) != 16 || first.ID != second.ID || first.ID == other.ID || first.Label != "opencode" {
		t.Fatalf("bad identity: %+v %+v %+v", first, second, other)
	}
	if IdentifyCallerValue("").ID != "anonymous" {
		t.Fatal("empty auth must be anonymous")
	}
	if IdentifyCallerValue("Basic abc").ID != "anonymous" {
		t.Fatal("foreign auth must be anonymous")
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-opencode")
	if IdentifyCaller(headers).Label != "opencode" {
		t.Fatal("header lookup failed")
	}
}

func TestFamilyOf(t *testing.T) {
	cases := map[string]string{
		"Qwen3.8-Max": "Qwen",
		"kimi-k3":     "Kimi",
		"deepseek":    "DeepSeek",
		"glm-5":       "GLM",
		"unknown-x":   "Other",
	}
	for code, want := range cases {
		if got := FamilyOf(code); got != want {
			t.Fatalf("%q -> %q", code, got)
		}
	}
}

func TestFormatTokens(t *testing.T) {
	cases := map[float64]string{
		0:       "0",
		999:     "999",
		1200:    "1.2K",
		100000:  "100K",
		2500000: "2.5M",
	}
	for value, want := range cases {
		if got := FormatTokens(value); got != want {
			t.Fatalf("%v -> %q want %q", value, got, want)
		}
	}
}

func TestExtractUsage(t *testing.T) {
	cases := []struct {
		name     string
		node     string
		ok       bool
		prompt   int
		compl    int
		total    int
		cached   int
		reported bool
	}{
		{"plain", `{"usage":{"prompt_tokens":10,"completion_tokens":5}}`, true, 10, 5, 15, 0, false},
		{"input output names", `{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":99}}`, true, 10, 5, 99, 0, false},
		{"details cache", `{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":7}}}`, true, 10, 5, 15, 7, true},
		{"input details cache", `{"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":3}}}`, true, 10, 5, 15, 3, true},
		{"cache hit field", `{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_cache_hit_tokens":4}}`, true, 10, 5, 15, 4, true},
		{"anthropic field", `{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":6}}`, true, 10, 5, 15, 6, true},
		{"nested response", `{"response":{"usage":{"input_tokens":10,"output_tokens":5}}}`, true, 10, 5, 15, 0, false},
		{"cached clamped", `{"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":99}}}`, true, 10, 5, 15, 10, true},
		{"string numbers", `{"usage":{"prompt_tokens":"12","completion_tokens":"3"}}`, true, 12, 3, 15, 0, false},
		{"missing", `{"usage":{}}`, false, 0, 0, 0, 0, false},
		{"not object", `[]`, false, 0, 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var node any
			if err := json.Unmarshal([]byte(tc.node), &node); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			usage, ok := ExtractUsage(node)
			if ok != tc.ok {
				t.Fatalf("ok = %v", ok)
			}
			if !ok {
				return
			}
			if usage.PromptTokens != tc.prompt || usage.CompletionTokens != tc.compl || usage.TotalTokens != tc.total ||
				usage.CachedPromptTokens != tc.cached || usage.CacheReported != tc.reported {
				t.Fatalf("got %+v", usage)
			}
		})
	}
}

func TestSSEUsageTracker(t *testing.T) {
	tracker := &SSEUsageTracker{}
	tracker.Push("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":12,")
	if tracker.Usage() != nil {
		t.Fatal("usage should not appear before the line is complete")
	}
	tracker.Push("\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	usage := tracker.Usage()
	if usage == nil || usage.PromptTokens != 12 || usage.CompletionTokens != 3 {
		t.Fatalf("got %+v", usage)
	}
}

func TestSSEUsageTrackerLongLine(t *testing.T) {
	tracker := &SSEUsageTracker{}
	longText := strings.Repeat("x", 300_000)
	line := "data: {\"choices\":[{\"delta\":{\"content\":\"" + longText + "\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n"
	tracker.Push(line)
	usage := tracker.Usage()
	if usage == nil || usage.PromptTokens != 1 || usage.CompletionTokens != 2 {
		t.Fatalf("long line usage lost: %+v", usage)
	}
}

func TestSSEUsageTrackerOversizedLineDropped(t *testing.T) {
	tracker := &SSEUsageTracker{}
	tracker.Push("data: " + strings.Repeat("y", MaxSSELineBytes+10))
	tracker.Push("\n\ndata: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n")
	if usage := tracker.Usage(); usage == nil || usage.PromptTokens != 4 {
		t.Fatalf("got %+v", usage)
	}
}

func TestCallerIdentityModel(t *testing.T) {
	var _ model.CallerIdentity = model.AnonymousCaller
}
