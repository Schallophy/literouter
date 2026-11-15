// Package stream implements response text extraction, in-memory live output,
// response-id chaining and the event log.
package stream

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Schallophy/literouter/internal/model"
)

// OutputToolCall is a display-only tool invocation; arguments stay raw JSON.
type OutputToolCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ExtractOutputText pulls display text out of a complete non-streaming body.
func ExtractOutputText(api model.ApiKind, data any) *string {
	record, ok := asRecord(data)
	if !ok {
		return nil
	}
	switch api {
	case model.ApiChat:
		return chatOutput(record)
	case model.ApiCompletions:
		return completionsOutput(record)
	case model.ApiResponses:
		return responsesOutput(record)
	case model.ApiEmbeddings:
		return embeddingsOutput(record)
	}
	return nil
}

// ExtractToolCalls pulls tool calls out of a complete non-streaming body.
func ExtractToolCalls(api model.ApiKind, data any) []OutputToolCall {
	record, ok := asRecord(data)
	if !ok {
		return nil
	}
	switch api {
	case model.ApiChat:
		return chatToolCalls(record)
	case model.ApiResponses:
		return responsesToolCalls(record)
	}
	return nil
}

// SseOutputExtractor turns SSE increments into plain text, buffering partial
// lines and events across chunk boundaries.
type SseOutputExtractor struct {
	api          model.ApiKind
	buffer       string
	hasText      bool
	finishReason *string
	toolCalls    map[string]*toolAcc
	nextOrder    int
}

type toolAcc struct {
	name  string
	args  string
	order int
}

// NewSSEOutputExtractor builds an extractor for one response.
func NewSSEOutputExtractor(api model.ApiKind) *SseOutputExtractor {
	return &SseOutputExtractor{api: api, toolCalls: map[string]*toolAcc{}}
}

// Push consumes a chunk and returns newly visible text.
func (e *SseOutputExtractor) Push(chunk string) string {
	e.buffer += chunk
	var out strings.Builder
	for {
		index := eventSeparatorIndex(e.buffer)
		if index < 0 {
			break
		}
		// Find the length of the separator as well.
		sepLen := 2
		if index+1 < len(e.buffer) && e.buffer[index] == '\r' {
			sepLen = 4
		} else if index+1 < len(e.buffer) && e.buffer[index] == '\n' && e.buffer[index+1] == '\r' {
			sepLen = 3
		}
		block := e.buffer[:index]
		e.buffer = e.buffer[index+sepLen:]
		text := e.block(block)
		if text != "" {
			e.hasText = true
			out.WriteString(text)
		}
	}
	return out.String()
}

// eventSeparatorIndex finds the first \r?\n\r?\n and returns its start.
func eventSeparatorIndex(text string) int {
	for index := 0; index+1 < len(text); index++ {
		if text[index] == '\n' && text[index+1] == '\n' {
			return index
		}
		if text[index] == '\r' && text[index+1] == '\n' {
			if index+2 < len(text) && text[index+2] == '\n' {
				return index
			}
			if index+2 < len(text) && text[index+2] == '\r' && index+3 < len(text) && text[index+3] == '\n' {
				return index
			}
		}
	}
	return -1
}

// Flush parses any trailing block that lacked a final blank line.
func (e *SseOutputExtractor) Flush() string {
	block := e.buffer
	e.buffer = ""
	if strings.TrimSpace(block) == "" {
		return ""
	}
	text := e.block(block)
	if text != "" {
		e.hasText = true
	}
	return text
}

// FinishReason reports the protocol finish reason, if any.
func (e *SseOutputExtractor) FinishReason() *string { return e.finishReason }

// ToolCalls returns accumulated tool calls in output order.
func (e *SseOutputExtractor) ToolCalls() []OutputToolCall {
	accs := make([]*toolAcc, 0, len(e.toolCalls))
	for _, acc := range e.toolCalls {
		if acc.name != "" {
			accs = append(accs, acc)
		}
	}
	sort.SliceStable(accs, func(i, j int) bool { return accs[i].order < accs[j].order })
	calls := make([]OutputToolCall, 0, len(accs))
	for _, acc := range accs {
		calls = append(calls, OutputToolCall{Name: acc.name, Arguments: acc.args})
	}
	return calls
}

func (e *SseOutputExtractor) setFinish(reason string) {
	if reason != "" {
		e.finishReason = &reason
	}
}

func (e *SseOutputExtractor) acc(key string, order int) *toolAcc {
	if entry, ok := e.toolCalls[key]; ok {
		return entry
	}
	entry := &toolAcc{order: order}
	e.toolCalls[key] = entry
	return entry
}

func (e *SseOutputExtractor) collectChatToolCalls(value any) {
	list, ok := value.([]any)
	if !ok {
		return
	}
	for _, entry := range list {
		call, ok := asRecord(entry)
		if !ok {
			continue
		}
		index := 0
		if number, ok := intValue(call["index"]); ok {
			index = number
		}
		acc := e.acc(fmt.Sprintf("chat:%d", index), index)
		if fn, ok := asRecord(call["function"]); ok {
			if name, ok := fn["name"].(string); ok && name != "" && acc.name == "" {
				acc.name = name
			}
			if args, ok := fn["arguments"].(string); ok {
				acc.args += args
			}
		}
	}
}

func (e *SseOutputExtractor) block(block string) string {
	var lines []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimLeft(line[5:], " \t"))
		}
	}
	data := strings.TrimSpace(strings.Join(lines, "\n"))
	if data == "" || data == "[DONE]" {
		return ""
	}
	var payload any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return ""
	}
	record, ok := asRecord(payload)
	if !ok {
		return ""
	}

	switch e.api {
	case model.ApiChat:
		choices, _ := record["choices"].([]any)
		var out strings.Builder
		for _, choiceValue := range choices {
			choice, ok := asRecord(choiceValue)
			if !ok {
				continue
			}
			delta, ok := asRecord(choice["delta"])
			if !ok {
				continue
			}
			out.WriteString(textFromContent(delta["content"]))
			e.collectChatToolCalls(delta["tool_calls"])
			if reason, ok := choice["finish_reason"].(string); ok {
				e.setFinish(reason)
			}
		}
		return out.String()

	case model.ApiCompletions:
		choices, _ := record["choices"].([]any)
		var out strings.Builder
		for _, choiceValue := range choices {
			choice, ok := asRecord(choiceValue)
			if !ok {
				continue
			}
			if text, ok := choice["text"].(string); ok {
				out.WriteString(text)
			}
			if reason, ok := choice["finish_reason"].(string); ok {
				e.setFinish(reason)
			}
		}
		return out.String()

	case model.ApiResponses:
		return e.responsesBlock(record)
	}
	return ""
}

func (e *SseOutputExtractor) responsesBlock(record map[string]any) string {
	eventType, _ := record["type"].(string)

	switch eventType {
	case "response.output_item.added", "response.output_item.done":
		if item, ok := asRecord(record["item"]); ok && item["type"] == "function_call" {
			key, ok := responseCallKey(record)
			if !ok {
				key = "responses:#0"
			}
			acc := e.acc(key, responseCallOrder(record))
			if name, ok := item["name"].(string); ok && name != "" {
				acc.name = name
			}
			if args, ok := item["arguments"].(string); ok && args != "" {
				acc.args = args
			}
		}
	case "response.function_call_arguments.delta":
		key, ok := responseCallKey(record)
		if !ok {
			key = "responses:#0"
		}
		if delta, ok := record["delta"].(string); ok && delta != "" {
			acc := e.acc(key, responseCallOrder(record))
			acc.args += delta
		}
	case "response.function_call_arguments.done":
		key, ok := responseCallKey(record)
		if !ok {
			key = "responses:#0"
		}
		if args, ok := record["arguments"].(string); ok {
			acc := e.acc(key, responseCallOrder(record))
			acc.args = args
		}
	}

	if delta, ok := record["delta"].(string); ok {
		if strings.Contains(eventType, "output_text.delta") || strings.Contains(eventType, "refusal.delta") {
			return delta
		}
	}

	if eventType == "response.output_text.done" {
		e.setFinish("done")
		if !e.hasText {
			if text, ok := record["text"].(string); ok {
				return text
			}
		}
		return ""
	}

	if eventType == "response.completed" {
		e.setFinish("completed")
		if len(e.toolCalls) == 0 {
			if response, ok := asRecord(record["response"]); ok {
				for index, call := range responsesToolCalls(response) {
					key := fmt.Sprintf("responses:completed:%d", index)
					e.toolCalls[key] = &toolAcc{name: call.Name, args: call.Arguments, order: index}
				}
			}
		}
		if !e.hasText {
			if response, ok := asRecord(record["response"]); ok {
				if text := responsesOutput(response); text != nil {
					return *text
				}
			}
		}
	}
	return ""
}

func responseCallKey(record map[string]any) (string, bool) {
	if itemID, ok := record["item_id"].(string); ok && itemID != "" {
		return "responses:" + itemID, true
	}
	if item, ok := asRecord(record["item"]); ok {
		if id, ok := item["id"].(string); ok && id != "" {
			return "responses:" + id, true
		}
	}
	if index, ok := intValue(record["output_index"]); ok {
		return fmt.Sprintf("responses:#%d", index), true
	}
	return "", false
}

func responseCallOrder(record map[string]any) int {
	if index, ok := intValue(record["output_index"]); ok {
		return index
	}
	return 0
}

func chatOutput(record map[string]any) *string {
	choices, _ := record["choices"].([]any)
	var parts []string
	for _, choiceValue := range choices {
		choice, ok := asRecord(choiceValue)
		if !ok {
			continue
		}
		if message, ok := asRecord(choice["message"]); ok {
			if content := textFromContent(message["content"]); content != "" {
				parts = append(parts, content)
			}
		}
		if text, ok := choice["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	joined := strings.Join(parts, "\n")
	return &joined
}

func completionsOutput(record map[string]any) *string {
	choices, _ := record["choices"].([]any)
	var out strings.Builder
	for _, choiceValue := range choices {
		if choice, ok := asRecord(choiceValue); ok {
			if text, ok := choice["text"].(string); ok {
				out.WriteString(text)
			}
		}
	}
	if out.Len() == 0 {
		return nil
	}
	text := out.String()
	return &text
}

func responsesOutput(record map[string]any) *string {
	if text, ok := record["output_text"].(string); ok && text != "" {
		return &text
	}
	output, _ := record["output"].([]any)
	var parts []string
	for _, item := range output {
		if text, ok := item.(string); ok {
			parts = append(parts, text)
			continue
		}
		entry, ok := asRecord(item)
		if !ok {
			continue
		}
		if content := textFromContent(entry["content"]); content != "" {
			parts = append(parts, content)
		}
		if text, ok := entry["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	joined := strings.Join(parts, "\n")
	return &joined
}

func embeddingsOutput(record map[string]any) *string {
	data, _ := record["data"].([]any)
	if len(data) == 0 {
		return nil
	}
	dimensions := -1
	if first, ok := asRecord(data[0]); ok {
		if vector, ok := first["embedding"].([]any); ok {
			dimensions = len(vector)
		}
	}
	text := fmt.Sprintf("%d 条向量", len(data))
	if dimensions >= 0 {
		text += fmt.Sprintf(" × %d 维", dimensions)
	}
	return &text
}

func chatToolCalls(record map[string]any) []OutputToolCall {
	var calls []OutputToolCall
	choices, _ := record["choices"].([]any)
	for _, choiceValue := range choices {
		choice, ok := asRecord(choiceValue)
		if !ok {
			continue
		}
		message, ok := asRecord(choice["message"])
		if !ok {
			continue
		}
		list, ok := message["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, callValue := range list {
			call, ok := asRecord(callValue)
			if !ok {
				continue
			}
			fn, ok := asRecord(call["function"])
			if !ok {
				continue
			}
			name, ok := fn["name"].(string)
			if !ok || name == "" {
				continue
			}
			args, _ := fn["arguments"].(string)
			calls = append(calls, OutputToolCall{Name: name, Arguments: args})
		}
	}
	return calls
}

func responsesToolCalls(record map[string]any) []OutputToolCall {
	var calls []OutputToolCall
	output, _ := record["output"].([]any)
	for _, itemValue := range output {
		entry, ok := asRecord(itemValue)
		if !ok || entry["type"] != "function_call" {
			continue
		}
		name, ok := entry["name"].(string)
		if !ok || name == "" {
			continue
		}
		args, _ := entry["arguments"].(string)
		calls = append(calls, OutputToolCall{Name: name, Arguments: args})
	}
	return calls
}

func textFromContent(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	var out strings.Builder
	for _, part := range parts {
		if text, ok := part.(string); ok {
			out.WriteString(text)
			continue
		}
		record, ok := asRecord(part)
		if !ok {
			continue
		}
		if text, ok := record["text"].(string); ok {
			out.WriteString(text)
			continue
		}
		if text, ok := record["content"].(string); ok {
			out.WriteString(text)
		}
	}
	return out.String()
}

func asRecord(value any) (map[string]any, bool) {
	record, ok := value.(map[string]any)
	return record, ok
}

func intValue(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		number, err := typed.Int64()
		return int(number), err == nil
	}
	return 0, false
}
