package httpapi

import (
	"errors"
	"os"
	"strings"

	"github.com/Schallophy/literouter/internal/sessions"
)

func (s *Server) registerSessions() {
	s.add("GET", "/api/meta", s.handleMeta)
	s.add("GET", "/api/sessions", s.handleSessions)
	s.add("GET", "/api/session", s.handleSession)
	s.add("POST", "/api/handoff", s.handleHandoff)
	s.add("POST", "/api/shutdown", s.handleShutdown)
}

func (s *Server) handleMeta(ctx *Ctx) error {
	ctx.Send(200, map[string]any{
		"app":      "literouter",
		"version":  Version,
		"home":     s.Sessions.Home(),
		"reasons":  reasonOptions(),
		"defaults": map[string]any{"limit": 40000},
	}, map[string]string{"cache-control": "no-store"})
	return nil
}

// reasonOptions is what the handoff reason picker offers. An empty code means
// "keep whatever the store detected".
func reasonOptions() []map[string]string {
	return []map[string]string{
		{"code": sessions.ReasonQuota, "label": "额度用完"},
		{"code": sessions.ReasonUpstream, "label": "上游报错"},
		{"code": sessions.ReasonAborted, "label": "被中断"},
		{"code": sessions.ReasonSwitch, "label": "主动换人"},
		{"code": sessions.ReasonOther, "label": "其他原因"},
	}
}

func (s *Server) handleSessions(ctx *Ctx) error {
	if ctx.URL.Query().Get("refresh") == "1" {
		ctx.Send(200, s.Sessions.Refresh(), map[string]string{"cache-control": "no-store"})
		return nil
	}
	ctx.Send(200, s.Sessions.List(), map[string]string{"cache-control": "no-store"})
	return nil
}

func (s *Server) handleSession(ctx *Ctx) error {
	agent := strings.TrimSpace(ctx.URL.Query().Get("agent"))
	id := strings.TrimSpace(ctx.URL.Query().Get("id"))
	if agent == "" || id == "" {
		return &HttpError{Status: 400, Message: "缺少 agent 或 id"}
	}
	result, err := s.Sessions.Detail(agent, id)
	if err != nil {
		return sessionError(err)
	}
	ctx.Send(200, result, map[string]string{"cache-control": "no-store"})
	return nil
}

func (s *Server) handleHandoff(ctx *Ctx) error {
	body, _ := ctx.Body.(map[string]any)
	agent := strings.TrimSpace(stringValue(body, "agent"))
	id := strings.TrimSpace(stringValue(body, "id"))
	if agent == "" || id == "" {
		return &HttpError{Status: 400, Message: "缺少 agent 或 id"}
	}
	result, err := s.Sessions.Handoff(agent, id, sessions.HandoffOptions{
		Note:     stringValue(body, "note"),
		Reason:   reasonCode(stringValue(body, "reason")),
		Summary:  boolValue(body, "summary", true),
		Messages: boolValue(body, "messages", true),
		Todos:    boolValue(body, "todos", true),
		Full:     boolValue(body, "full", false),
		Limit:    intValue(body, "limit", 40000),
	})
	if err != nil {
		return sessionError(err)
	}
	ctx.Send(200, result, map[string]string{"cache-control": "no-store"})
	return nil
}

// handleShutdown lets `literouter stop` end a background instance. The token
// comes from the state file; a page cannot set a custom header cross-origin,
// so the dashboard cannot stop the server it is rendered from.
func (s *Server) handleShutdown(ctx *Ctx) error {
	if s.Token == "" || s.Stop == nil {
		return &HttpError{Status: 404, Message: "该实例不支持远程退出"}
	}
	if !strings.EqualFold(ctx.R.Header.Get("x-literouter-token"), s.Token) {
		return &HttpError{Status: 403, Message: "令牌不匹配"}
	}
	ctx.Send(200, map[string]any{"stopping": true}, nil)
	go s.Stop()
	return nil
}

func sessionError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return &HttpError{Status: 404, Message: "会话不存在或已被删除"}
	}
	return &HttpError{Status: 502, Message: err.Error()}
}

func reasonCode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "quota":
		return sessions.ReasonQuota
	case "upstream":
		return sessions.ReasonUpstream
	case "aborted":
		return sessions.ReasonAborted
	case "switch":
		return sessions.ReasonSwitch
	case "other":
		return sessions.ReasonOther
	}
	return sessions.ReasonDetected
}

func stringValue(body map[string]any, key string) string {
	value, _ := body[key].(string)
	return value
}

func boolValue(body map[string]any, key string, fallback bool) bool {
	value, ok := body[key].(bool)
	if !ok {
		return fallback
	}
	return value
}

func intValue(body map[string]any, key string, fallback int) int {
	value, ok := body[key].(float64)
	if !ok {
		return fallback
	}
	return int(value)
}
