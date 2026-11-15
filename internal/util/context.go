package util

import (
	"encoding/json"
	"math"
	"unicode"
)

// ContextEstimate is the prompt/output estimate used by the overflow guard.
type ContextEstimate struct {
	PromptTokens int `json:"promptTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

const safetyMargin = 1.03

// IsCJK reports whether a rune falls in the CJK ranges counted as one token.
func IsCJK(r rune) bool {
	return (r >= 0x3000 && r <= 0x303f) ||
		(r >= 0x3040 && r <= 0x30ff) ||
		(r >= 0x3400 && r <= 0x4dbf) ||
		(r >= 0x4e00 && r <= 0x9fff) ||
		(r >= 0xac00 && r <= 0xd7af) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xff00 && r <= 0xffef) ||
		(r >= 0x20000 && r <= 0x2fa1f)
}

func isLetter(r rune) bool { return unicode.IsLetter(r) && !IsCJK(r) }
func isNumber(r rune) bool { return unicode.IsDigit(r) }
func isMark(r rune) bool   { return unicode.IsMark(r) }
func isSpace(r rune) bool  { return unicode.IsSpace(r) }

func isPictograph(r rune) bool {
	// Extended_Pictographic and regional indicators, approximated by the main
	// emoji blocks used in practice.
	return (r >= 0x1f000 && r <= 0x1faff) || (r >= 0x2600 && r <= 0x27bf) || (r >= 0x1f1e6 && r <= 0x1f1ff)
}

func naturalWordTokens(length int) float64 {
	switch {
	case length <= 8:
		return 1
	case length <= 12:
		return 1.5
	case length <= 16:
		return 2.5
	default:
		return float64(length) / 5
	}
}

func wordTokens(letters []rune) float64 {
	parts := 1
	start := 0
	total := 0.0
	for index := 1; index < len(letters); index++ {
		prev := letters[index-1]
		current := letters[index]
		if (unicode.IsLower(prev) || unicode.IsDigit(prev)) && unicode.IsUpper(current) {
			total += naturalWordTokens(index - start)
			start = index
			parts++
		}
	}
	total += naturalWordTokens(len(letters) - start)
	_ = parts
	return total
}

func symbolTokens(piece []rune) float64 {
	tokens := 0.0
	run := 0
	flush := func() {
		if run > 0 {
			tokens += math.Max(1, math.Round(float64(run)/7))
			run = 0
		}
	}
	for _, r := range piece {
		if r == 0x200d || isMark(r) || isSpace(r) {
			continue
		}
		if isPictograph(r) {
			flush()
			tokens += 2
		} else {
			run++
		}
	}
	flush()
	return tokens
}

// EstimateTokens approximates a tokenizer count following cl100k/o200k shape.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	runes := []rune(text)
	tokens := 0.0
	index := 0
	for index < len(runes) {
		r := runes[index]
		if IsCJK(r) {
			tokens++
			index++
			continue
		}
		if isNumber(r) {
			count := 0
			for index < len(runes) && isNumber(runes[index]) && count < 3 {
				index++
				count++
			}
			tokens++
			continue
		}
		if isSpace(r) || r == '\r' || r == '\n' {
			start := index
			for index < len(runes) && (isSpace(runes[index]) || runes[index] == '\r' || runes[index] == '\n') {
				index++
			}
			runLength := index - start
			if runLength == 1 && runes[start] != '\r' && runes[start] != '\n' && index < len(runes) {
				next := runes[index]
				// A single space/tab joins the following word or symbol run.
				if isLetter(next) || isSymbolRune(next) {
					continue
				}
			}
			tokens++
			continue
		}
		if isLetter(r) {
			start := index
			for index < len(runes) && isLetter(runes[index]) {
				index++
			}
			tokens += wordTokens(runes[start:index])
			continue
		}
		// One leading symbol joins a word ("_case"); otherwise it is a symbol run.
		if r != ' ' && index+1 < len(runes) && isLetter(runes[index+1]) {
			index++
			start := index
			for index < len(runes) && isLetter(runes[index]) {
				index++
			}
			tokens += wordTokens(runes[start:index])
			continue
		}
		start := index
		for index < len(runes) && isSymbolRune(runes[index]) {
			index++
		}
		tokens += symbolTokens(runes[start:index])
	}
	if tokens <= 0 {
		return 0
	}
	return int(math.Max(1, math.Round(tokens*safetyMargin)))
}

func isSymbolRune(r rune) bool {
	return !isLetter(r) && !isNumber(r) && !isSpace(r) && r != '\r' && r != '\n' && !IsCJK(r)
}

func contentTokens(content any) int {
	switch typed := content.(type) {
	case string:
		return EstimateTokens(typed)
	case []any:
		total := 0
		for _, part := range typed {
			switch item := part.(type) {
			case string:
				total += EstimateTokens(item)
			case map[string]any:
				itemType, _ := item["type"].(string)
				switch {
				case itemType == "image_url" || itemType == "image":
					total += 256
				case itemType == "input_audio":
					total += 512
				case isString(item["text"]):
					total += EstimateTokens(item["text"].(string))
				case isString(item["content"]):
					total += EstimateTokens(item["content"].(string))
				case item["content"] != nil:
					if nested, ok := item["content"].([]any); ok {
						total += contentTokens(nested)
					}
				case isString(item["output"]):
					total += EstimateTokens(item["output"].(string))
				case isString(item["arguments"]):
					total += EstimateTokens(item["arguments"].(string))
				}
			}
		}
		return total
	}
	return 0
}

func isString(value any) bool {
	_, ok := value.(string)
	return ok
}

func countJSON(value any) int {
	if value == nil {
		return 0
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return EstimateTokens(string(encoded))
}

func readOutputTokens(body map[string]any) int {
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if number, ok := body[key].(float64); ok && number > 0 && !math.IsInf(number, 0) && !math.IsNaN(number) {
			return int(math.Floor(number))
		}
	}
	return 0
}

// EstimateRequestTokens estimates prompt and requested output tokens.
func EstimateRequestTokens(path string, body map[string]any) ContextEstimate {
	promptTokens := 0
	if path == "responses" {
		promptTokens += 3
		promptTokens += contentTokens(body["input"])
		promptTokens += contentTokens(body["instructions"])
		promptTokens += countJSON(body["tools"])
		outputTokens := readOutputTokens(body)
		return ContextEstimate{PromptTokens: promptTokens, OutputTokens: outputTokens, TotalTokens: promptTokens + outputTokens}
	}

	if path == "completions" {
		promptTokens = contentTokens(body["prompt"]) + 2
	} else {
		messages, _ := body["messages"].([]any)
		for _, message := range messages {
			item, ok := message.(map[string]any)
			if !ok {
				continue
			}
			promptTokens += 3
			if role, ok := item["role"].(string); ok {
				promptTokens += EstimateTokens(role)
			}
			if name, ok := item["name"].(string); ok {
				promptTokens += EstimateTokens(name)
			}
			promptTokens += contentTokens(item["content"])
			promptTokens += countJSON(item["tool_calls"])
			promptTokens += countJSON(item["function_call"])
		}
		promptTokens += 3
	}

	promptTokens += countJSON(body["tools"])
	promptTokens += countJSON(body["functions"])
	if body["response_format"] != nil {
		promptTokens += countJSON(body["response_format"])
	}
	outputTokens := readOutputTokens(body)
	return ContextEstimate{PromptTokens: promptTokens, OutputTokens: outputTokens, TotalTokens: promptTokens + outputTokens}
}

// ApplyCalibration scales a prompt estimate by a learned ratio.
func ApplyCalibration(promptTokens int, ratio float64) int {
	if ratio <= 0 {
		ratio = 1
	}
	return int(math.Ceil(float64(promptTokens) * ratio))
}

// IsOverflow reports whether the calibrated estimate exceeds the window.
func IsOverflow(estimate ContextEstimate, ratio float64, window int) bool {
	return ApplyCalibration(estimate.TotalTokens, ratio) > window
}
