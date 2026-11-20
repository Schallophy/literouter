package httpapi

import (
	"errors"
	"os"
	"strings"
)

func (s *Server) registerSessions() {
	s.add("GET", "/api/sessions", s.handleSessions)
	s.add("GET", "/api/sessions/handoff", s.handleSessionHandoff)
	s.add("GET", "/api/sessions/detail", s.handleSessionDetail)
}

func (s *Server) handleSessions(ctx *Ctx) error {
	if ctx.URL.Query().Get("refresh") == "1" {
		ctx.Send(200, s.Sessions.Refresh(), map[string]string{"cache-control": "no-store"})
		return nil
	}
	ctx.Send(200, s.Sessions.List(), map[string]string{"cache-control": "no-store"})
	return nil
}

func (s *Server) handleSessionHandoff(ctx *Ctx) error {
	agent := strings.TrimSpace(ctx.URL.Query().Get("agent"))
	id := strings.TrimSpace(ctx.URL.Query().Get("id"))
	if agent == "" || id == "" {
		return &HttpError{Status: 400, Message: "缺少 agent 或 id"}
	}
	result, err := s.Sessions.Handoff(agent, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &HttpError{Status: 404, Message: "会话不存在或已被删除"}
		}
		return &HttpError{Status: 502, Message: err.Error()}
	}
	ctx.Send(200, result, map[string]string{"cache-control": "no-store"})
	return nil
}

func (s *Server) handleSessionDetail(ctx *Ctx) error {
	agent := strings.TrimSpace(ctx.URL.Query().Get("agent"))
	id := strings.TrimSpace(ctx.URL.Query().Get("id"))
	if agent == "" || id == "" {
		return &HttpError{Status: 400, Message: "缺少 agent 或 id"}
	}
	result, err := s.Sessions.Detail(agent, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &HttpError{Status: 404, Message: "会话不存在或已被删除"}
		}
		return &HttpError{Status: 502, Message: err.Error()}
	}
	ctx.Send(200, result, map[string]string{"cache-control": "no-store"})
	return nil
}
