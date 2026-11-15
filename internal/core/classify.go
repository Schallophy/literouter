package core

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Schallophy/literouter/internal/model"
)

// ErrorPayload is the provider's error code/message pair.
type ErrorPayload struct {
	Code    *string
	Message string
}

const maxMessageRunes = 600

var (
	freeTierPattern       = regexp.MustCompile(`(?i)FreeTierOnly|free[\s_-]*tier|free[\s_-]*quota|试用额度|免费额度`)
	allocationQuota       = regexp.MustCompile(`^AllocationQuota`)
	throttlePattern       = regexp.MustCompile(`(?i)Throttling|RateLimit|rate[\s_-]*limit|too many requests|请求过于频繁|限流`)
	authPattern           = regexp.MustCompile(`InvalidApiKey|Authentication|Unauthorized|InvalidAccessKey|SignatureDoesNotMatch|InvalidCredential`)
	modelMissingPattern   = regexp.MustCompile(`(?i)ModelNotFound|ModelNotExist|model.*(does not exist|not exist|not found|invalid|not supported)`)
	permissionPattern     = regexp.MustCompile(`(?i)AccessDenied|Forbidden|not\s*authori[sz]ed|not\s*activated|ModelNotOpen|no\s*permission`)
	whitespaceRun         = regexp.MustCompile(`\s+`)
)

// CleanMessage collapses whitespace, trims, and caps the message at 600 runes.
func CleanMessage(text string) string {
	cleaned := strings.TrimSpace(whitespaceRun.ReplaceAllString(text, " "))
	runes := []rune(cleaned)
	if len(runes) > maxMessageRunes {
		cleaned = string(runes[:maxMessageRunes])
	}
	return cleaned
}

// ParseErrorPayload extracts a code and message from any provider error body.
func ParseErrorPayload(raw string) ErrorPayload {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ErrorPayload{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return ErrorPayload{Message: CleanMessage(text)}
	}
	root, ok := parsed.(map[string]any)
	if !ok {
		return ErrorPayload{Message: CleanMessage(text)}
	}
	var code *string
	message := ""

	switch nested := root["error"].(type) {
	case string:
		message = nested
	case map[string]any:
		if value, ok := nested["code"].(string); ok {
			code = &value
		}
		if value, ok := nested["message"].(string); ok {
			message = value
		}
		if code == nil {
			if value, ok := nested["type"].(string); ok {
				code = &value
			}
		}
	}

	if code == nil {
		if value, ok := root["code"].(string); ok {
			code = &value
		}
	}
	if message == "" {
		if value, ok := root["message"].(string); ok {
			message = value
		}
	}
	if message == "" {
		if value, ok := root["msg"].(string); ok {
			message = value
		}
	}
	if message == "" {
		if value, ok := root["detail"].(string); ok {
			message = value
		}
	}
	if message == "" && code != nil {
		message = *code
	}
	if message == "" {
		message = text
	}
	return ErrorPayload{Code: code, Message: CleanMessage(message)}
}

type errorFlags struct {
	fatal       bool
	markDead    bool
	markInvalid bool
	retryable   bool
}

func buildError(kind model.FailureKind, label string, status *int, payload ErrorPayload, flags errorFlags) model.NormalizedError {
	return model.NormalizedError{
		Kind:        kind,
		Label:       label,
		Status:      status,
		Code:        payload.Code,
		Message:     payload.Message,
		Fatal:       flags.fatal,
		MarkDead:    flags.markDead,
		MarkInvalid: flags.markInvalid,
		Retryable:   flags.retryable,
	}
}

// ClassifyHTTPError maps a provider HTTP status and payload to the gateway's
// uniform failure classification, preserving the original precedence.
func ClassifyHTTPError(status int, payload ErrorPayload) model.NormalizedError {
	code := ""
	if payload.Code != nil {
		code = *payload.Code
	}
	blob := code + " " + payload.Message
	statusValue := status

	if status == 429 || throttlePattern.MatchString(blob) {
		return buildError(model.FailureThrottled, "触发限流", &statusValue, payload, errorFlags{retryable: true})
	}
	if status == 403 && (freeTierPattern.MatchString(blob) || allocationQuota.MatchString(code)) {
		return buildError(model.FailureQuotaExhausted, "额度已耗尽", &statusValue, payload, errorFlags{markDead: true, retryable: true})
	}
	if status == 401 || authPattern.MatchString(blob) {
		return buildError(model.FailureAuth, "API Key 无效", &statusValue, payload, errorFlags{fatal: true})
	}
	if modelMissingPattern.MatchString(blob) {
		return buildError(model.FailureModelInvalid, "模型不可用", &statusValue, payload, errorFlags{markInvalid: true, retryable: true})
	}
	if status == 403 {
		label := "请求被拒绝"
		if permissionPattern.MatchString(blob) {
			label = "无访问权限"
		}
		return buildError(model.FailureForbidden, label, &statusValue, payload, errorFlags{retryable: true})
	}
	if status == 404 {
		return buildError(model.FailureProvider, "提供商地址异常", &statusValue, payload, errorFlags{retryable: true})
	}
	if status == 400 || status == 422 {
		return buildError(model.FailureBadRequest, "请求参数错误", &statusValue, payload, errorFlags{retryable: true})
	}
	if status >= 500 {
		return buildError(model.FailureProvider, "提供商服务异常", &statusValue, payload, errorFlags{retryable: true})
	}
	return buildError(model.FailureUnknown, "未知错误", &statusValue, payload, errorFlags{retryable: true})
}

// ClassifyNetworkError handles timeouts, client aborts and transport failures.
func ClassifyNetworkError(err error, timedOut, clientAborted bool) model.NormalizedError {
	if clientAborted {
		message := "client aborted"
		return buildError(model.FailureAborted, "请求已中断", nil, ErrorPayload{Message: message}, errorFlags{fatal: true})
	}
	if timedOut {
		return buildError(model.FailureTimeout, "请求超时", nil, ErrorPayload{Message: "provider timeout"}, errorFlags{retryable: true})
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	return buildError(model.FailureNetwork, "网络错误", nil, ErrorPayload{Message: CleanMessage(message)}, errorFlags{retryable: true})
}
