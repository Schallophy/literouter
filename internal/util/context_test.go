package util

import "testing"

func TestEstimateTokens(t *testing.T) {
	cases := map[string]int{
		"你好世界":                  4,
		"":                      0,
		"   ":                   1,
		"hello":                 1,
		"hello world":           2,
		"The quick brown fox":   4,
		"a b c":                 3,
		"Respond with JSON.":    4,
		"1234567890":            4,
		"3.141592653589793":     7,
		"hello, world!":         4,
		"--- === ***":           3,
		"getUserName":           3,
		"XMLHttpRequest":        2,
		"snake_case_identifier": 4,
		"用 React 写一个登录页":        9,
	}
	for text, want := range cases {
		if got := EstimateTokens(text); got != want {
			t.Fatalf("%q -> %d want %d", text, got, want)
		}
	}
}

func TestEstimateRequestTokens(t *testing.T) {
	chat := EstimateRequestTokens("chat/completions", map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "你好"}},
		"max_tokens": 100.0,
	})
	if chat.PromptTokens != 9 || chat.OutputTokens != 100 || chat.TotalTokens != 109 {
		t.Fatalf("chat: %+v", chat)
	}

	completion := EstimateRequestTokens("chat/completions", map[string]any{
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		"max_completion_tokens": 50.0,
	})
	if completion.PromptTokens != 8 || completion.OutputTokens != 50 || completion.TotalTokens != 58 {
		t.Fatalf("completion: %+v", completion)
	}

	image := EstimateRequestTokens("chat/completions", map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "hi"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
			},
		}},
	})
	if image.PromptTokens != 264 {
		t.Fatalf("image: %+v", image)
	}

	completions := EstimateRequestTokens("completions", map[string]any{"prompt": "hello"})
	if completions.PromptTokens != 3 || completions.OutputTokens != 0 {
		t.Fatalf("completions: %+v", completions)
	}

	withoutTools := EstimateRequestTokens("chat/completions", map[string]any{"messages": []any{}})
	withTools := EstimateRequestTokens("chat/completions", map[string]any{
		"messages": []any{},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "lookup", "description": "look up a record"},
		}},
	})
	if withTools.PromptTokens <= withoutTools.PromptTokens {
		t.Fatalf("tools not counted")
	}

	responses := EstimateRequestTokens("responses", map[string]any{
		"instructions":      "讲中文",
		"input":             "你好",
		"max_output_tokens": 64.0,
	})
	if responses.PromptTokens != 8 || responses.OutputTokens != 64 || responses.TotalTokens != 72 {
		t.Fatalf("responses: %+v", responses)
	}

	items := EstimateRequestTokens("responses", map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "lookup done"},
		},
	})
	if items.PromptTokens != 6 || items.OutputTokens != 0 {
		t.Fatalf("items: %+v", items)
	}
}

func TestApplyCalibration(t *testing.T) {
	if got := ApplyCalibration(100, 1.5); got != 150 {
		t.Fatalf("got %d", got)
	}
	if got := ApplyCalibration(100, 0); got != 100 {
		t.Fatalf("got %d", got)
	}
	if !IsOverflow(ContextEstimate{TotalTokens: 700}, 1.5, 1000) {
		t.Fatal("should overflow")
	}
	if IsOverflow(ContextEstimate{TotalTokens: 700}, 1, 1000) {
		t.Fatal("should fit")
	}
}
