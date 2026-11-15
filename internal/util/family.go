package util

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

type familyRule struct {
	pattern *regexp.Regexp
	name    string
}

var families = []familyRule{
	{regexp.MustCompile(`(?i)^qwen`), "Qwen"},
	{regexp.MustCompile(`(?i)^kimi`), "Kimi"},
	{regexp.MustCompile(`(?i)^deepseek`), "DeepSeek"},
	{regexp.MustCompile(`(?i)^glm`), "GLM"},
	{regexp.MustCompile(`(?i)^llama`), "Llama"},
	{regexp.MustCompile(`(?i)^mistral`), "Mistral"},
	{regexp.MustCompile(`(?i)^ernie`), "ERNIE"},
	{regexp.MustCompile(`(?i)^doubao`), "Doubao"},
	{regexp.MustCompile(`(?i)^gpt`), "GPT"},
	{regexp.MustCompile(`(?i)^claude`), "Claude"},
	{regexp.MustCompile(`(?i)^gemini`), "Gemini"},
	{regexp.MustCompile(`(?i)^moonshot`), "Moonshot"},
	{regexp.MustCompile(`(?i)^minimax`), "MiniMax"},
	{regexp.MustCompile(`(?i)^baichuan`), "Baichuan"},
	{regexp.MustCompile(`(?i)^yi`), "Yi"},
}

// FamilyOf classifies a model code into its vendor family.
func FamilyOf(code string) string {
	for _, rule := range families {
		if rule.pattern.MatchString(code) {
			return rule.name
		}
	}
	return "Other"
}

// FormatTokens renders 1200 as "1.2K" and 2500000 as "2.5M".
func FormatTokens(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "0"
	}
	absolute := math.Abs(value)
	if absolute >= 1_000_000 {
		return trimNumber(value/1_000_000) + "M"
	}
	if absolute >= 1_000 {
		return trimNumber(value/1_000) + "K"
	}
	return strconv.FormatInt(int64(math.Round(value)), 10)
}

func trimNumber(value float64) string {
	if value >= 100 {
		return strings.TrimSuffix(fmt.Sprintf("%.0f", value), ".0")
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0")
}
