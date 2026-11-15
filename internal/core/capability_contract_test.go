package core

import (
	"encoding/json"
	"fmt"
	"testing"
)

func capModel(slug string, score *float64, options ...func(*CapabilityModel)) CapabilityModel {
	entry := CapabilityModel{Slug: slug, Name: slug, Score: score}
	for _, option := range options {
		option(&entry)
	}
	return entry
}

func score(value float64) *float64 { return &value }

func leaderboardHTML(models []CapabilityModel) string {
	records := make([]map[string]any, 0, len(models))
	for _, model := range models {
		record := map[string]any{
			"slug":                model.Slug,
			"name":                model.Name,
			"modelCreatorName":    model.Creator,
			"intelligenceIndex":   model.Score,
			"contextWindowTokens": model.ContextWindow,
			"isReasoning":         model.Reasoning,
		}
		records = append(records, record)
	}
	encoded, _ := json.Marshal(records)
	payload := fmt.Sprintf(`22:["$","div",null,{"initialModels":%s}]`, encoded)
	split := len(payload) / 2
	first, _ := json.Marshal(payload[:split])
	second, _ := json.Marshal(payload[split:])
	return `<script>self.__next_f.push([1,` + string(first) + `])</script>` +
		`<script>self.__next_f.push([1,` + string(second) + `])</script>`
}

func TestCapabilityContract(t *testing.T) {
	creator := "Anthropic"
	contextWindow := 200_000
	parsed := ParseLeaderboardHTML(leaderboardHTML([]CapabilityModel{
		capModel("claude-opus-5-5", score(59.1), func(entry *CapabilityModel) {
			entry.Name = "Claude Opus 5.5"
			entry.Creator = &creator
			entry.ContextWindow = &contextWindow
			entry.Reasoning = true
		}),
		capModel("unscored-model", nil),
	}))
	t.Run("parses models from the Next.js flight payload", func(t *testing.T) {
		if len(parsed) != 2 {
			t.Fatalf("got %d models", len(parsed))
		}
		first := parsed[0]
		if first.Slug != "claude-opus-5-5" || first.Name != "Claude Opus 5.5" || first.Creator == nil || *first.Creator != "Anthropic" ||
			first.Score == nil || *first.Score != 59.1 || first.ContextWindow == nil || *first.ContextWindow != 200_000 || !first.Reasoning {
			t.Fatalf("got %+v", first)
		}
		if parsed[1].Score != nil {
			t.Fatalf("unscored: %+v", parsed[1])
		}
	})

	t.Run("a malformed flight chunk does not hide the rest of the page", func(t *testing.T) {
		good := leaderboardHTML([]CapabilityModel{capModel("claude-opus-5-5", score(59.1))})
		html := `<script>self.__next_f.push([1,"not json"])</script>` + good
		if got := ParseLeaderboardHTML(html); len(got) != 1 {
			t.Fatalf("got %d", len(got))
		}
		if got := ParseLeaderboardHTML("<html></html>"); len(got) != 0 {
			t.Fatalf("empty page: %+v", got)
		}
	})

	models := []CapabilityModel{
		capModel("deepseek-v4-pro", score(23.1)),
		capModel("deepseek-v4-1-flash-1017", score(32.6)),
		capModel("deepseek-v4-1-flash-1017-non-reasoning", score(18.5)),
		capModel("claude-opus-5-5", score(59.1)),
		capModel("claude-opus-5-5-low", score(34.8)),
		capModel("gemini-3-5-flash-lite", score(11.7)),
		capModel("glm-5-3", score(31.7)),
		capModel("deepseek-v4-flash-vision", score(8.4)),
		capModel("minimax-m3", score(18.9)),
		capModel("unscored-model", nil),
	}
	index := BuildCapabilityIndex(models)
	slugFor := func(name string) string {
		match, ok := MatchCapability(index, name)
		if !ok {
			return ""
		}
		return match.Model.Slug
	}

	t.Run("matches exact slugs and provider-prefixed upstream names", func(t *testing.T) {
		cases := map[string]string{
			"claude-opus-5-5":          "claude-opus-5-5",
			"deepseek/deepseek-v4-pro": "deepseek-v4-pro",
			"MiniMax/MiniMax-M3":       "minimax-m3",
		}
		for name, want := range cases {
			if got := slugFor(name); got != want {
				t.Fatalf("%q -> %q want %q", name, got, want)
			}
		}
	})

	t.Run("matches dots, dates and non-capability suffixes", func(t *testing.T) {
		cases := map[string]string{
			"deepseek-v4.1-flash":            "deepseek-v4-1-flash-1017",
			"deepseek-v4.1-flash-1017":       "deepseek-v4-1-flash-1017",
			"deepseek-v4.1-flash-free":       "deepseek-v4-1-flash-1017",
			"deepseek-v4-pro-0813":           "deepseek-v4-pro",
			"deepseek/deepseek-v4-pro:batch": "deepseek-v4-pro",
		}
		for name, want := range cases {
			if got := slugFor(name); got != want {
				t.Fatalf("%q -> %q want %q", name, got, want)
			}
		}
	})

	t.Run("uses the highest reasoning effort when the catalog has no effort suffix", func(t *testing.T) {
		match, ok := MatchCapability(index, "claude-opus-5-5")
		if !ok || match.Model.Slug != "claude-opus-5-5" || match.Model.Score == nil || *match.Model.Score != 59.1 {
			t.Fatalf("got %+v ok=%v", match, ok)
		}
	})

	t.Run("keeps an explicit effort variant distinct", func(t *testing.T) {
		match, ok := MatchCapability(index, "claude-opus-5-5-low")
		if !ok || match.Model.Slug != "claude-opus-5-5-low" || match.Model.Score == nil || *match.Model.Score != 34.8 || match.Cost != 0 {
			t.Fatalf("got %+v ok=%v", match, ok)
		}
	})

	t.Run("does not match a different version or a different model family", func(t *testing.T) {
		for _, name := range []string{"glm-5", "gemini-3.5-flash", "deepseek-v4-flash", "unscored-model", ""} {
			if got := slugFor(name); got != "" {
				t.Fatalf("%q unexpectedly matched %q", name, got)
			}
		}
	})

	expanded := []CapabilityModel{
		capModel("nvidia-nemotron-3-ultra-550b-a55b", score(11.8)),
		capModel("tencent-hy4", score(34.6)),
		capModel("gemma-4-31b", score(4.4)),
		capModel("kimi-k3-1-pro", score(46.5)),
		capModel("mimo-v2-6-pro", score(29.9)),
		capModel("mistral-large-4", score(26.1)),
		capModel("muse-glimmer", score(6.3)),
		capModel("ling-3-1-flash", nil),
		capModel("ling-3-1-flash-1019", score(29.6)),
		capModel("command-a", nil),
		capModel("command-a-plus", score(6.2)),
		capModel("alpha", nil),
		capModel("alpha-preview", score(5)),
		capModel("gpt-oss-20b", nil),
		capModel("gpt-oss-120b", score(3.8)),
		capModel("claude-opus-5-5", score(59.1)),
		capModel("glm-5-3", score(31.7)),
		capModel("deepseek-r1", score(20)),
	}
	expandedIndex := BuildCapabilityIndex(expanded)
	expandedSlug := func(name string) string {
		match, ok := MatchCapability(expandedIndex, name)
		if !ok {
			return ""
		}
		return match.Model.Slug
	}

	t.Run("matches vendor prefixes in AA slugs", func(t *testing.T) {
		cases := map[string]string{
			"nvidia/nemotron-3-ultra-550b-a55b":      "nvidia-nemotron-3-ultra-550b-a55b",
			"nvidia/nemotron-3-ultra-550b-a55b:free": "nvidia-nemotron-3-ultra-550b-a55b",
			"tencent/hy4":                            "tencent-hy4",
			"tencent/hy4-preview":                    "tencent-hy4",
		}
		for name, want := range cases {
			if got := expandedSlug(name); got != want {
				t.Fatalf("%q -> %q want %q", name, got, want)
			}
		}
	})

	t.Run("matches instruction and provider speed suffixes", func(t *testing.T) {
		cases := map[string]string{
			"google/gemma-4-31b-it":           "gemma-4-31b",
			"kimi/kimi-k3.1-pro-highspeed":    "kimi-k3-1-pro",
			"xiaomi/mimo-v2.6-pro-ultraspeed": "mimo-v2-6-pro",
		}
		for name, want := range cases {
			if got := expandedSlug(name); got != want {
				t.Fatalf("%q -> %q want %q", name, got, want)
			}
		}
	})

	t.Run("matches a single trailing zero version", func(t *testing.T) {
		if got := expandedSlug("mistralai/mistral-large-4-0"); got != "mistral-large-4" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("drops a size suffix only when it does not contain version digits", func(t *testing.T) {
		cases := map[string]string{
			"meta/muse-glimmer-30b":       "muse-glimmer",
			"meta/muse-glimmer-30b:batch": "muse-glimmer",
			"qwen3-14b":                   "",
			"deepseek-r1-8b":              "",
		}
		for name, want := range cases {
			if got := expandedSlug(name); got != want {
				t.Fatalf("%q -> %q want %q", name, got, want)
			}
		}
	})

	t.Run("keeps an unscored date snapshot matched to its scored variant", func(t *testing.T) {
		if got := expandedSlug("inclusionai/ling-3.1-flash"); got != "ling-3-1-flash-1019" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("does not fall through an unscored exact match to a different model", func(t *testing.T) {
		cases := map[string]string{
			"cohere/command-a":      "",
			"cohere/command-a-plus": "command-a-plus",
			"alpha":                 "",
			"alpha-preview":         "alpha-preview",
			"openai/gpt-oss-20b":    "",
			"openai/gpt-oss-120b":   "gpt-oss-120b",
		}
		for name, want := range cases {
			if got := expandedSlug(name); got != want {
				t.Fatalf("%q -> %q want %q", name, got, want)
			}
		}
	})

	t.Run("does not guess across major versions", func(t *testing.T) {
		for _, name := range []string{"claude-opus-4-5", "glm-4.5", "deepseek-r1-8b"} {
			if got := expandedSlug(name); got != "" {
				t.Fatalf("%q unexpectedly matched %q", name, got)
			}
		}
	})
}
