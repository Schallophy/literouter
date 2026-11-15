package core

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Schallophy/literouter/internal/model"
)

// AttemptOutcome is the result of one provider attempt.
type AttemptOutcome struct {
	OK         bool
	Response   *http.Response
	LatencyMs  int64
	ProviderID string
	Error      model.NormalizedError
}

// FailoverHooks observe each failed attempt by classification.
type FailoverHooks struct {
	OnDead    func(modelCode string, err model.NormalizedError)
	OnInvalid func(modelCode string, err model.NormalizedError)
	OnThrottle func(modelCode string, err model.NormalizedError)
	OnSkip    func(modelCode string, err model.NormalizedError)
}

// Attemptor performs one provider request.
type Attemptor func(ctx context.Context, modelCode string) AttemptOutcome

// FailoverInput drives RunFailover.
type FailoverInput struct {
	Context     context.Context
	Candidates  []string
	MaxAttempts int
	Attempt     Attemptor
	Hooks       FailoverHooks
}

// FailoverResult reports the winning response or the final error.
type FailoverResult struct {
	OK         bool
	Model      string
	Response   *http.Response
	Attempts   []model.AttemptRecord
	Failovers  int
	LatencyMs  int64
	Provider   string
	Error      model.NormalizedError
	Exhausted  bool
}

func abortedError() model.NormalizedError {
	message := "client aborted"
	return model.NormalizedError{
		Kind:    model.FailureAborted,
		Label:   "请求已中断",
		Message: message,
		Fatal:   true,
	}
}

// ExhaustedError summarizes every failed attempt in the final error message.
func ExhaustedError(attempts []model.AttemptRecord) model.NormalizedError {
	parts := make([]string, 0, len(attempts))
	allThrottled := len(attempts) > 0
	for _, item := range attempts {
		kind := string(item.Kind)
		if kind == "" {
			kind = "error"
		}
		part := fmt.Sprintf("%s: %s", item.Model, kind)
		if item.Status != nil {
			part += fmt.Sprintf(" %d", *item.Status)
		}
		parts = append(parts, part)
		if item.Kind != model.FailureThrottled {
			allThrottled = false
		}
	}
	code := "literouter.exhausted"
	kind := model.FailureExhausted
	label := "所有模型均不可用"
	if allThrottled {
		kind = model.FailureThrottled
		label = "所有模型均被限流"
	}
	message := "没有可用的模型"
	if len(parts) > 0 {
		message = fmt.Sprintf("没有可用的模型（%s）", strings.Join(parts, "; "))
	}
	return model.NormalizedError{
		Kind:    kind,
		Label:   label,
		Code:    &code,
		Message: message,
		Fatal:   true,
	}
}

func intPtr(value int) *int { return &value }

func stringPtr(value string) *string { return &value }

// RunFailover walks candidates in priority order until one succeeds, a fatal
// error occurs, or the attempt budget is exhausted.
func RunFailover(input FailoverInput) FailoverResult {
	ctx := input.Context
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	attempts := make([]model.AttemptRecord, 0, len(input.Candidates))
	candidates := make([]string, 0, len(input.Candidates))
	for _, code := range input.Candidates {
		if code != "" {
			candidates = append(candidates, code)
		}
	}
	limit := input.MaxAttempts
	if limit > len(candidates) {
		limit = len(candidates)
	}
	if limit < 1 {
		limit = 1
	}

	failovers := func() int {
		if len(attempts) > 0 {
			return len(attempts) - 1
		}
		return 0
	}

	for index := 0; index < limit; index++ {
		if index >= len(candidates) {
			break
		}
		modelCode := candidates[index]
		if err := ctx.Err(); err != nil {
			return FailoverResult{
				Attempts:  attempts,
				Failovers: failovers(),
				LatencyMs: time.Since(started).Milliseconds(),
				Error:     abortedError(),
			}
		}

		outcome := input.Attempt(ctx, modelCode)
		record := model.AttemptRecord{
			Model:     modelCode,
			OK:        outcome.OK,
			LatencyMs: outcome.LatencyMs,
		}
		if !outcome.OK {
			record.Kind = outcome.Error.Kind
			record.Status = outcome.Error.Status
			record.Code = outcome.Error.Code
			message := outcome.Error.Message
			record.Message = &message
		}
		attempts = append(attempts, record)

		if outcome.OK {
			return FailoverResult{
				OK:        true,
				Model:     modelCode,
				Response:  outcome.Response,
				Attempts:  attempts,
				Failovers: len(attempts) - 1,
				LatencyMs: time.Since(started).Milliseconds(),
				Provider:  outcome.ProviderID,
			}
		}

		switch outcome.Error.Kind {
		case model.FailureQuotaExhausted:
			if input.Hooks.OnDead != nil {
				input.Hooks.OnDead(modelCode, outcome.Error)
			}
		case model.FailureModelInvalid:
			if input.Hooks.OnInvalid != nil {
				input.Hooks.OnInvalid(modelCode, outcome.Error)
			}
		case model.FailureThrottled:
			if input.Hooks.OnThrottle != nil {
				input.Hooks.OnThrottle(modelCode, outcome.Error)
			}
		default:
			if input.Hooks.OnSkip != nil {
				input.Hooks.OnSkip(modelCode, outcome.Error)
			}
		}

		if outcome.Error.Fatal {
			return FailoverResult{
				Attempts:  attempts,
				Failovers: failovers(),
				LatencyMs: time.Since(started).Milliseconds(),
				Error:     outcome.Error,
			}
		}
	}

	return FailoverResult{
		Attempts:  attempts,
		Failovers: failovers(),
		LatencyMs: time.Since(started).Milliseconds(),
		Error:     ExhaustedError(attempts),
		Exhausted: true,
	}
}
