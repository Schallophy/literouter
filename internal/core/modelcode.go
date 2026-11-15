package core

import (
	"strings"

	"github.com/Schallophy/literouter/internal/model"
)

// ParsedModelCode is a model code split into provider id and upstream name.
type ParsedModelCode struct {
	Provider string
	Upstream string
}

// SplitModelCode resolves "providerId/upstream" against known providers by
// longest provider-id prefix, so upstream names may themselves contain slashes.
func SplitModelCode(code string, providers []model.ProviderConfig) (ParsedModelCode, bool) {
	return splitModelCodeIDs(code, providerIDs(providers))
}

func providerIDs(providers []model.ProviderConfig) []string {
	ids := make([]string, 0, len(providers))
	for _, p := range providers {
		ids = append(ids, p.ID)
	}
	return ids
}

func splitModelCodeIDs(code string, ids []string) (ParsedModelCode, bool) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return ParsedModelCode{}, false
	}
	best := ""
	for _, id := range ids {
		if id == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, id+"/") {
			continue
		}
		if len(id) > len(best) {
			best = id
		}
	}
	if best == "" {
		return ParsedModelCode{}, false
	}
	upstream := strings.TrimSpace(trimmed[len(best)+1:])
	if upstream == "" {
		return ParsedModelCode{}, false
	}
	return ParsedModelCode{Provider: best, Upstream: upstream}, true
}

// JoinModelCode builds the canonical "provider/upstream" code.
func JoinModelCode(providerID, upstream string) string {
	return strings.TrimSpace(providerID) + "/" + strings.TrimLeft(strings.TrimSpace(upstream), "/")
}
