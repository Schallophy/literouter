package core

import (
	"strings"
	"time"

	"github.com/Schallophy/literouter/internal/model"
)

// RouteMode describes how a requested model name resolves against the chain.
type RouteMode string

const (
	RouteAuto        RouteMode = "auto"
	RouteDirect      RouteMode = "direct"
	RoutePassthrough RouteMode = "passthrough"
)

// RouteResolution is the outcome of resolving "model" from a request body.
type RouteResolution struct {
	Mode  RouteMode
	Model string
}

// ResolveRoute maps auto / listed models / bare upstream names to a route.
// A bare upstream name only matches when it is unique across the chain.
func ResolveRoute(requested string, chain model.ChainConfig, providers []model.ProviderConfig) RouteResolution {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" || trimmed == "auto" {
		first := ""
		if len(chain.Models) > 0 {
			first = chain.Models[0]
		}
		return RouteResolution{Mode: RouteAuto, Model: first}
	}
	for _, code := range chain.Models {
		if code == trimmed {
			return RouteResolution{Mode: RouteDirect, Model: trimmed}
		}
	}
	if len(providers) > 0 {
		matches := make([]string, 0, 1)
		for _, code := range chain.Models {
			parsed, ok := SplitModelCode(code, providers)
			if ok && parsed.Upstream == trimmed {
				matches = append(matches, code)
			}
		}
		if len(matches) == 1 {
			return RouteResolution{Mode: RouteDirect, Model: matches[0]}
		}
	}
	return RouteResolution{Mode: RoutePassthrough, Model: trimmed}
}

// Candidate is a chain entry that may serve the request.
type Candidate struct {
	Code   string
	Record *model.ModelRecord
}

// IsDeadRetryDue reports whether a dead model's retry window has elapsed.
func IsDeadRetryDue(record *model.ModelRecord, now int64, retryMs int64) bool {
	if record == nil || record.Health != model.HealthDead || retryMs <= 0 {
		return false
	}
	if record.CheckedAt == nil {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, *record.CheckedAt)
	if err != nil {
		return false
	}
	return now-at.UnixMilli() >= retryMs
}

// SelectionInput feeds SelectCandidates.
type SelectionInput struct {
	Chain           model.ChainConfig
	GetRecord       func(string) *model.ModelRecord
	Now             int64
	Accepts         func(string) bool
	RetryDeadAfterMs int64
}

// SelectCandidates walks the chain in priority order, skipping invalid models,
// dead models that are still waiting, and models in throttle cooldown.
func SelectCandidates(input SelectionInput) []Candidate {
	candidates := make([]Candidate, 0, len(input.Chain.Models))
	for _, code := range input.Chain.Models {
		record := input.GetRecord(code)
		if record == nil {
			continue
		}
		if input.Accepts != nil && !input.Accepts(code) {
			continue
		}
		if record.Health == model.HealthInvalid {
			continue
		}
		if record.Health == model.HealthDead && !IsDeadRetryDue(record, input.Now, input.RetryDeadAfterMs) {
			continue
		}
		if IsThrottled(record, input.Now) {
			continue
		}
		candidates = append(candidates, Candidate{Code: code, Record: record})
	}
	return candidates
}
