package sessions

import (
	"regexp"
	"strings"
)

var (
	systemReminderPattern = regexp.MustCompile(`(?is)<system-reminder\b[^>]*>.*?</system-reminder>`)
	userInfoPattern       = regexp.MustCompile(`(?is)<user_info\b[^>]*>.*?</user_info>`)
	wrapperTagPattern     = regexp.MustCompile(`(?i)</?(user_query|long_text_quote|long-text|user-context|environment)\b[^>]*>`)
	zcodeCwdPattern       = regexp.MustCompile(`(?m)Primary working directory:\s*(.+)`)
)

// classify converts an agent's own error signal into a Status.
//
// The mapping is deliberately conservative: a status we cannot name is still
// "stuck", because the user reached the UI precisely because the session will
// not continue.
func classify(kind, message string) Status {
	code := "unknown"
	label := "会话无法继续"
	lower := strings.ToLower(kind + " " + message)

	switch {
	case strings.Contains(lower, "free-limit") || strings.Contains(lower, "free usage limit") ||
		strings.Contains(lower, "quota") || strings.Contains(lower, "insufficient"):
		code, label = "free-limit", "额度用完"
	case strings.Contains(lower, "rate") && strings.Contains(lower, "limit"),
		strings.Contains(lower, "429"):
		code, label = "rate-limit", "被限流"
	case strings.Contains(lower, "auth") || strings.Contains(lower, "401") ||
		strings.Contains(lower, "api key") || strings.Contains(lower, "apiKey"):
		code, label = "auth", "认证失效"
	case strings.Contains(lower, "abort"):
		code, label = "interrupted", "被中断"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline"):
		code, label = "timeout", "请求超时"
	case strings.Contains(lower, "transport") || strings.Contains(lower, "econn") ||
		strings.Contains(lower, "socket") || strings.Contains(lower, "connection"):
		code, label = "transport", "连接中断"
	// `provider.invalid-request` is frequently a wrapper around an upstream
	// 5xx, so the provider's own wording wins over the class name.
	case strings.Contains(lower, "upstream") || strings.Contains(lower, "500") ||
		strings.Contains(lower, "502") || strings.Contains(lower, "503") ||
		strings.Contains(lower, "provider."):
		code, label = "upstream", "上游报错"
	case strings.Contains(lower, "invalid-request") || strings.Contains(lower, "invalid_request"):
		code, label = "invalid", "请求被拒绝"
	}

	detail := strings.TrimSpace(message)
	if genericDetail(detail) {
		// "Aborted" / "error" adds nothing next to the label the row already
		// shows; only a real provider sentence is worth the space.
		detail = ""
	}
	if len([]rune(detail)) > 120 {
		detail = string([]rune(detail)[:120]) + "…"
	}
	return Status{State: "stuck", Code: code, Label: label, Detail: detail}
}

// genericDetail reports whether a provider message is just the name of a
// condition rather than an explanation of it.
func genericDetail(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimSuffix(value, ".")
	switch value {
	case "", "abort", "aborted", "interrupt", "interrupted", "error", "failed",
		"failure", "unknown", "canceled", "cancelled", "timeout":
		return true
	}
	return false
}

// done builds the status for a session that finished its last turn normally.
func done() Status {
	return Status{State: "done"} // 已完成
}

// CompactDetail shortens the provider detail for a list row or CLI line.
func (s Status) CompactDetail() string {
	detail := strings.TrimSpace(s.Detail)
	if detail == "" {
		return ""
	}
	detail = strings.Join(strings.Fields(detail), " ")
	if len([]rune(detail)) > 64 {
		return string([]rune(detail)[:64]) + "…"
	}
	return detail
}

// trimDetail bounds a provider message kept for display.
func trimDetail(value string) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > 160 {
		return string([]rune(value)[:160]) + "…"
	}
	return value
}
