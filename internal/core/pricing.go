package core

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Schallophy/literouter/internal/model"
)

var clockPattern = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

func clockMinutes(value string) (int, bool) {
	match := clockPattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, false
	}
	hour, _ := strconv.Atoi(match[1])
	minute, _ := strconv.Atoi(match[2])
	return hour*60 + minute, true
}

func minutesInZone(at time.Time, timeZone string) int {
	location, err := time.LoadLocation(timeZone)
	if err != nil {
		location = time.UTC
	}
	local := at.In(location)
	return local.Hour()*60 + local.Minute()
}

// PriceTierAt selects the tier for a model at a given instant. The model's own
// timezone decides the clock; a missing model entry falls back to "*".
func PriceTierAt(pricing model.PricingConfig, modelCode string, at time.Time) (model.PriceTier, bool) {
	entry, ok := pricing.Models[modelCode]
	if !ok {
		entry, ok = pricing.Models["*"]
	}
	if !ok {
		return model.PriceTier{}, false
	}
	zone := strings.TrimSpace(entry.Timezone)
	if zone == "" {
		zone = "UTC"
	}
	minutes := minutesInZone(at, zone)
	for _, tier := range entry.Tiers {
		from, fromOK := clockMinutes(tier.From)
		to, toOK := clockMinutes(tier.To)
		if !fromOK || !toOK {
			continue
		}
		if from == to {
			return tier, true
		}
		if from < to {
			if minutes >= from && minutes < to {
				return tier, true
			}
		} else if minutes >= from || minutes < to {
			return tier, true
		}
	}
	return model.PriceTier{}, false
}

// UsageCostParts splits a usage block into billed classes. Cached input uses
// the cachedInput price; the remainder uses input; output uses output.
func UsageCostParts(usage model.Usage, tier model.PriceTier, unit int64) model.CostSplit {
	if unit <= 0 {
		return model.CostSplit{}
	}
	cached := clampInt(usage.CachedPromptTokens, 0, maxInt(0, usage.PromptTokens))
	fresh := maxInt(0, usage.PromptTokens-cached)
	output := maxInt(0, usage.CompletionTokens)
	savedRate := maxFloat(0, tier.Input-tier.CachedInput)
	divisor := float64(unit)
	return model.CostSplit{
		Input:       float64(fresh) * tier.Input / divisor,
		CachedInput: float64(cached) * tier.CachedInput / divisor,
		Output:      float64(output) * tier.Output / divisor,
		CacheSaved:  float64(cached) * savedRate / divisor,
	}
}

func UsageCost(usage model.Usage, tier model.PriceTier, unit int64) float64 {
	parts := UsageCostParts(usage, tier, unit)
	return parts.Total()
}

// CostForUsage computes the cost at a given time, or zero if the model has no
// usable price entry.
func CostForUsage(pricing model.PricingConfig, modelCode string, usage model.Usage, at time.Time) (model.CostSplit, bool) {
	tier, ok := PriceTierAt(pricing, modelCode, at)
	if !ok {
		return model.CostSplit{}, false
	}
	return UsageCostParts(usage, tier, pricing.Unit), true
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
