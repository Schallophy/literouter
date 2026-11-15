package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Schallophy/literouter/internal/model"
	"github.com/Schallophy/literouter/internal/runtime"
	"github.com/Schallophy/literouter/internal/stream"
	"github.com/Schallophy/literouter/internal/util"
)

type modelItem struct {
	ID            string        `json:"id"`
	Object        string        `json:"object"`
	Created       int64         `json:"created"`
	OwnedBy       string        `json:"owned_by"`
	Description   *string       `json:"description,omitempty"`
	ContextLength int           `json:"context_length"`
	APIs          []model.ApiKind `json:"apis"`
	Provider      string        `json:"provider"`
	Upstream      string        `json:"upstream,omitempty"`
	Active        any           `json:"active"`
}

func (s *Server) registerOpenAI() {
	s.add("GET", "/v1/models", s.handleModels)
	s.add("POST", "/v1/chat/completions", func(ctx *Ctx) error { return s.proxy(ctx, "chat/completions") })
	s.add("POST", "/v1/completions", func(ctx *Ctx) error { return s.proxy(ctx, "completions") })
	s.add("POST", "/v1/responses", func(ctx *Ctx) error { return s.proxy(ctx, "responses") })
	s.add("POST", "/v1/embeddings", func(ctx *Ctx) error { return s.proxy(ctx, "embeddings") })
}

func (s *Server) handleModels(ctx *Ctx) error {
	rt := s.Runtime
	cfg := rt.ConfigSnapshot()
	created := time.Now().Unix()
	primary := rt.PrimaryModel()

	apisOf := func(models []string) []model.ApiKind {
		seen := map[model.ApiKind]bool{}
		out := []model.ApiKind{}
		for _, code := range models {
			for _, api := range rt.ApisFor(code) {
				if !seen[api] {
					seen[api] = true
					out = append(out, api)
				}
			}
		}
		return out
	}

	description := "模型列表为空"
	if primary != nil {
		description = "从列表第一个模型开始，失败时按优先级回退"
	}
	autoProvider := cfg.DefaultProvider
	if primary != nil {
		autoProvider = rt.ProviderIDFor(*primary)
	}
	data := []modelItem{{
		ID:            "auto",
		Object:        "model",
		Created:       created,
		OwnedBy:       "literouter",
		Description:   &description,
		ContextLength: cfg.Chain.Context.Window,
		APIs:          apisOf(cfg.Chain.Models),
		Provider:      autoProvider,
		Active:        primary,
	}}

	for _, code := range rt.ChainCodes() {
		record := rt.Store.Get(code)
		if record == nil || record.Health == model.HealthDead || record.Health == model.HealthInvalid {
			continue
		}
		data = append(data, modelItem{
			ID:            code,
			Object:        "model",
			Created:       created,
			OwnedBy:       "literouter",
			ContextLength: cfg.Chain.Context.Window,
			APIs:          rt.ApisFor(code),
			Provider:      rt.ProviderIDFor(code),
			Upstream:      rt.UpstreamModelFor(code),
			Active:        primary != nil && code == *primary,
		})
	}

	ctx.Send(200, map[string]any{"object": "list", "data": data}, nil)
	return nil
}

func (s *Server) proxy(ctx *Ctx, path string) error {
	started := time.Now()
	body, ok := ctx.Body.(map[string]any)
	if !ok {
		return &HttpError{Status: 400, Message: "请求体必须是 JSON 对象"}
	}
	api := apiKindForPath(path)
	requested := "auto"
	if value, ok := body["model"].(string); ok {
		requested = value
	}
	isStream := body["stream"] == true
	caller := util.IdentifyCaller(ctx.R.Header)

	forward := make(map[string]any, len(body))
	for key, value := range body {
		if key != "model" {
			forward[key] = value
		}
	}
	if isStream && path == "chat/completions" {
		if providerCfg := s.Runtime.ProviderFor(requested); providerCfg != nil && providerCfg.InjectStreamUsage {
			if _, exists := forward["stream_options"]; !exists {
				forward["stream_options"] = map[string]any{"include_usage": true}
			}
		}
	}

	routed := s.Runtime.RequestThrough(runtime.RequestInput{
		Path:    path,
		Model:   requested,
		Body:    forward,
		Context: ctx.R.Context(),
		Caller:  caller,
	})
	s.Runtime.RecordOutcome(routed.Result, caller, api)

	if !routed.Result.OK {
		failure := routed.Result.Error
		failedModel := requested
		if len(routed.Result.Attempts) > 0 {
			failedModel = routed.Result.Attempts[len(routed.Result.Attempts)-1].Model
		}
		s.Runtime.LiveOutput.Start(stream.LiveOutputStart{
			API: api, Model: failedModel, Provider: providerPointer(s.Runtime, failedModel, ""),
			Caller: caller.Label, Requested: requested, Stream: isStream, Failovers: routed.Result.Failovers,
		}).Finish(stream.LiveOutputFinish{
			Error:     stringPointer(failure.Label + "：" + failure.Message),
			LatencyMs: int64Pointer(time.Since(started).Milliseconds()),
		})

		if failure.Kind == model.FailureAborted {
			panic(http.ErrAbortHandler)
		}
		status := errorStatus(failure)
		headers := map[string]string{}
		if status == 429 {
			headers["retry-after"] = "5"
		}
		if status == 503 {
			headers["retry-after"] = "30"
		}
		message := failure.Message
		if message == "" {
			message = failure.Label
		}
		code := "literouter." + string(failure.Kind)
		if failure.Code != nil && *failure.Code != "" {
			code = *failure.Code
		}
		ctx.Send(status, map[string]any{"error": map[string]any{
			"message":  message,
			"type":     "literouter." + string(failure.Kind),
			"code":     code,
			"param":    nil,
			"attempts": summarizeAttempts(routed.Result.Attempts),
		}}, headers)
		return nil
	}

	response := routed.Result.Response
	defer response.Body.Close()

	if !isStream {
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			parsed = nil
		}
		usage := usageOrNil(parsed)
		finishReason := extractFinishReason(api, parsed)
		s.Runtime.MarkSuccess(routed.Result.Model, usage, caller, routed.Sample, api)
		handle := s.Runtime.LiveOutput.Start(stream.LiveOutputStart{
			API: api, Model: routed.Result.Model,
			Provider:  providerPointer(s.Runtime, routed.Result.Model, routed.Result.Provider),
			Caller:    caller.Label,
			Requested: requested,
			Stream:    false,
			Failovers: routed.Result.Failovers,
		})
		rawText := string(raw)
		handle.Finish(stream.LiveOutputFinish{
			Text:         stream.ExtractOutputText(api, parsed),
			ToolCalls:    stream.ExtractToolCalls(api, parsed),
			FinishReason: finishReason,
			Raw:          &rawText,
			Usage:        usage,
			Bytes:        int64Pointer(int64(len(raw))),
			LatencyMs:    int64Pointer(time.Since(started).Milliseconds()),
		})
		ctx.SendRaw(200, raw, map[string]string{
			"content-type":          "application/json; charset=utf-8",
			"x-literouter-model":    routed.Result.Model,
			"x-literouter-provider": routed.Result.Provider,
			"x-literouter-failovers": fmt.Sprintf("%d", routed.Result.Failovers),
		})
		return nil
	}

	handle := s.Runtime.LiveOutput.Start(stream.LiveOutputStart{
		API: api, Model: routed.Result.Model,
		Provider:  providerPointer(s.Runtime, routed.Result.Model, routed.Result.Provider),
		Caller:    caller.Label,
		Requested: requested,
		Stream:    true,
		Failovers: routed.Result.Failovers,
	})

	piped := pipeSSE(ctx, response, pipeOptions{
		IdleTimeout: 120 * time.Second,
		Context:     ctx.R.Context(),
		Headers: map[string]string{
			"x-literouter-model":    routed.Result.Model,
			"x-literouter-provider": routed.Result.Provider,
			"x-literouter-failovers": fmt.Sprintf("%d", routed.Result.Failovers),
		},
		OnData: handle.Push,
	})
	s.Runtime.MarkSuccess(routed.Result.Model, piped.Usage, caller, routed.Sample, api)
	handle.Finish(stream.LiveOutputFinish{
		Usage:     piped.Usage,
		Bytes:     int64Pointer(piped.Bytes),
		LatencyMs: int64Pointer(time.Since(started).Milliseconds()),
		Error:     piped.Error,
	})
	if piped.Error != nil {
		s.Logger.Warn("stream interrupted", map[string]any{"model": routed.Result.Model, "error": *piped.Error})
	}
	return nil
}

func apiKindForPath(path string) model.ApiKind {
	switch path {
	case "responses":
		return model.ApiResponses
	case "completions":
		return model.ApiCompletions
	case "embeddings":
		return model.ApiEmbeddings
	default:
		return model.ApiChat
	}
}

func providerPointer(rt *runtime.Runtime, modelCode, providerID string) *string {
	if providerID != "" {
		value := providerID
		return &value
	}
	value := rt.ProviderIDFor(modelCode)
	return &value
}

func stringPointer(value string) *string { return &value }

func int64Pointer(value int64) *int64 { return &value }

func usageOrNil(data any) *model.Usage {
	usage, ok := util.ExtractUsage(data)
	if !ok {
		return nil
	}
	return &usage
}

func errorStatus(failure model.NormalizedError) int {
	switch failure.Kind {
	case model.FailureAuth:
		if failure.Code != nil && *failure.Code == "literouter.no_key" {
			return 503
		}
		return 401
	case model.FailureBadRequest, model.FailureContext:
		return 400
	case model.FailureForbidden:
		return 403
	case model.FailureThrottled:
		return 429
	case model.FailureProvider:
		return 502
	case model.FailureExhausted:
		return 503
	case model.FailureAborted:
		return 499
	default:
		return 503
	}
}

func extractFinishReason(api model.ApiKind, parsed any) *string {
	record, ok := parsed.(map[string]any)
	if !ok {
		return nil
	}
	if api == model.ApiChat || api == model.ApiCompletions {
		choices, _ := record["choices"].([]any)
		if len(choices) > 0 {
			if first, ok := choices[0].(map[string]any); ok {
				if reason, ok := first["finish_reason"].(string); ok && reason != "" {
					return &reason
				}
			}
		}
		return nil
	}
	if api == model.ApiResponses {
		output, _ := record["output"].([]any)
		if len(output) > 0 {
			if first, ok := output[0].(map[string]any); ok {
				if status, ok := first["status"].(string); ok && status != "" {
					return &status
				}
			}
		}
		if status, ok := record["status"].(string); ok && status != "" {
			return &status
		}
	}
	return nil
}

func summarizeAttempts(attempts []model.AttemptRecord) []map[string]any {	out := make([]map[string]any, 0, len(attempts))
	for _, attempt := range attempts {
		out = append(out, map[string]any{
			"model": attempt.Model, "ok": attempt.OK, "kind": attempt.Kind,
			"status": attempt.Status, "code": attempt.Code, "latencyMs": attempt.LatencyMs,
		})
	}
	return out
}
