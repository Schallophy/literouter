package util

import (
	"encoding/json"
	"strings"

	"github.com/Schallophy/literouter/internal/model"
)

// MaxSSELineBytes bounds a single data line; Responses completion events can
// carry tens of kilobytes of text.
const MaxSSELineBytes = 8 * 1024 * 1024

func numberOrNil(value any) *float64 {
	switch typed := value.(type) {
	case float64:
		return &typed
	case int:
		converted := float64(typed)
		return &converted
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		var parsed float64
		if _, err := fmtSscan(typed, &parsed); err != nil {
			return nil
		}
		return &parsed
	}
	return nil
}

func fmtSscan(text string, target *float64) (int, error) {
	var value float64
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &value); err != nil {
		return 0, err
	}
	*target = value
	return 1, nil
}

func objectOrNil(value any) map[string]any {
	if value == nil {
		return nil
	}
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return nil
}

func cacheTokensOf(fields map[string]any) *float64 {
	details := objectOrNil(fields["prompt_tokens_details"])
	if details == nil {
		details = objectOrNil(fields["input_tokens_details"])
	}
	if details != nil {
		if cached := numberOrNil(details["cached_tokens"]); cached != nil {
			return cached
		}
	}
	if cached := numberOrNil(fields["prompt_cache_hit_tokens"]); cached != nil {
		return cached
	}
	return numberOrNil(fields["cache_read_input_tokens"])
}

// ExtractUsage reads a usage block from a top-level node or response wrapper.
func ExtractUsage(node any) (model.Usage, bool) {
	record := objectOrNil(node)
	if record == nil {
		return model.Usage{}, false
	}
	usage := objectOrNil(record["usage"])
	if usage == nil {
		if response := objectOrNil(record["response"]); response != nil {
			usage = objectOrNil(response["usage"])
		}
	}
	if usage == nil {
		return model.Usage{}, false
	}

	prompt := numberOrNil(usage["prompt_tokens"])
	if prompt == nil {
		prompt = numberOrNil(usage["input_tokens"])
	}
	completion := numberOrNil(usage["completion_tokens"])
	if completion == nil {
		completion = numberOrNil(usage["output_tokens"])
	}
	if prompt == nil && completion == nil {
		return model.Usage{}, false
	}
	promptTokens := 0
	if prompt != nil {
		promptTokens = int(*prompt)
	}
	completionTokens := 0
	if completion != nil {
		completionTokens = int(*completion)
	}
	total := promptTokens + completionTokens
	if explicit := numberOrNil(usage["total_tokens"]); explicit != nil {
		total = int(*explicit)
	}
	cached := cacheTokensOf(usage)
	cachedTokens := 0
	reported := cached != nil
	if cached != nil {
		value := int(*cached)
		if value < 0 {
			value = 0
		}
		if value > promptTokens {
			value = promptTokens
		}
		cachedTokens = value
	}
	return model.Usage{
		PromptTokens:       promptTokens,
		CompletionTokens:   completionTokens,
		TotalTokens:        total,
		CachedPromptTokens: cachedTokens,
		CacheReported:      reported,
	}, true
}

// SSEUsageTracker scans SSE chunks for the last usage block.
type SSEUsageTracker struct {
	buffer   strings.Builder
	usage    *model.Usage
	skipLine bool
}

// Push feeds one chunk, consuming complete lines.
func (t *SSEUsageTracker) Push(chunk string) {
	t.buffer.WriteString(chunk)
	for {
		text := t.buffer.String()
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimSpace(text[:index])
		rest := text[index+1:]
		t.buffer.Reset()
		t.buffer.WriteString(rest)
		if !t.skipLine {
			t.consume(line)
		}
		t.skipLine = false
	}
	if t.buffer.Len() > MaxSSELineBytes {
		t.buffer.Reset()
		t.skipLine = true
	}
}

func (t *SSEUsageTracker) consume(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(line[5:])
	if payload == "" || payload == "[DONE]" {
		return
	}
	if !strings.Contains(payload, "usage") {
		return
	}
	var parsed any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return
	}
	if usage, ok := ExtractUsage(parsed); ok {
		t.usage = &usage
	}
}

// Usage returns the last usage block seen, or nil.
func (t *SSEUsageTracker) Usage() *model.Usage { return t.usage }
