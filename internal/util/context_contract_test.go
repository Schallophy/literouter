package util

import "testing"

func TestContextEstimationContract(t *testing.T) {
	t.Run("CJK characters count as one token each", func(t *testing.T) {
		for text, want := range map[string]int{"你好世界": 4, "": 0, "   ": 1} {
			if got := EstimateTokens(text); got != want {
				t.Fatalf("%q -> %d want %d", text, got, want)
			}
		}
	})
	t.Run("English text follows tokenizer-like word pieces", func(t *testing.T) {
		for text, want := range map[string]int{
			"hello": 1, "hello world": 2, "The quick brown fox": 4, "a b c": 3, "Respond with JSON.": 4,
		} {
			if got := EstimateTokens(text); got != want {
				t.Fatalf("%q -> %d want %d", text, got, want)
			}
		}
	})
	t.Run("numbers and punctuation use tokenizer-like grouping", func(t *testing.T) {
		for text, want := range map[string]int{
			"1234567890": 4, "3.141592653589793": 7, "hello, world!": 4, "--- === ***": 3,
		} {
			if got := EstimateTokens(text); got != want {
				t.Fatalf("%q -> %d want %d", text, got, want)
			}
		}
	})
	t.Run("identifiers are split on case boundaries", func(t *testing.T) {
		for text, want := range map[string]int{"getUserName": 3, "XMLHttpRequest": 2, "snake_case_identifier": 4} {
			if got := EstimateTokens(text); got != want {
				t.Fatalf("%q -> %d want %d", text, got, want)
			}
		}
	})
	t.Run("mixed CJK and Latin text keeps separate pieces", func(t *testing.T) {
		if got := EstimateTokens("用 React 写一个登录页"); got != 9 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("chat estimates include message overhead and max_tokens", func(t *testing.T) {
		estimate := EstimateRequestTokens("chat/completions", map[string]any{
			"messages":   []any{map[string]any{"role": "user", "content": "你好"}},
			"max_tokens": 100.0,
		})
		if estimate.PromptTokens != 9 || estimate.OutputTokens != 100 || estimate.TotalTokens != 109 {
			t.Fatalf("got %+v", estimate)
		}
	})
	t.Run("max_completion_tokens is used when max_tokens is absent", func(t *testing.T) {
		estimate := EstimateRequestTokens("chat/completions", map[string]any{
			"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
			"max_completion_tokens": 50.0,
		})
		if estimate.PromptTokens != 8 || estimate.OutputTokens != 50 || estimate.TotalTokens != 58 {
			t.Fatalf("got %+v", estimate)
		}
	})
	t.Run("image parts get a flat allowance", func(t *testing.T) {
		estimate := EstimateRequestTokens("chat/completions", map[string]any{
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "hi"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
				},
			}},
		})
		if estimate.PromptTokens != 264 {
			t.Fatalf("got %+v", estimate)
		}
	})
	t.Run("completions estimates use the prompt field", func(t *testing.T) {
		estimate := EstimateRequestTokens("completions", map[string]any{"prompt": "hello"})
		if estimate.PromptTokens != 3 || estimate.OutputTokens != 0 {
			t.Fatalf("got %+v", estimate)
		}
	})
	t.Run("tool definitions are counted toward the prompt", func(t *testing.T) {
		without := EstimateRequestTokens("chat/completions", map[string]any{"messages": []any{}})
		with := EstimateRequestTokens("chat/completions", map[string]any{
			"messages": []any{},
			"tools": []any{map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "lookup", "description": "look up a record"},
			}},
		})
		if with.PromptTokens <= without.PromptTokens {
			t.Fatalf("without=%+v with=%+v", without, with)
		}
	})
	t.Run("responses estimates read instructions, input and max_output_tokens", func(t *testing.T) {
		estimate := EstimateRequestTokens("responses", map[string]any{
			"instructions":      "讲中文",
			"input":             "你好",
			"max_output_tokens": 64.0,
		})
		if estimate.PromptTokens != 8 || estimate.OutputTokens != 64 || estimate.TotalTokens != 72 {
			t.Fatalf("got %+v", estimate)
		}
	})
	t.Run("responses input items count text and tool output", func(t *testing.T) {
		estimate := EstimateRequestTokens("responses", map[string]any{
			"input": []any{
				map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
				map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "lookup done"},
			},
		})
		if estimate.PromptTokens != 6 || estimate.OutputTokens != 0 {
			t.Fatalf("got %+v", estimate)
		}
	})
}
