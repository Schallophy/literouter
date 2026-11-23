package core

import (
	"math"
	"regexp"
	"sort"
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
	entry, ok := pricingEntry(pricing, modelCode)
	if !ok {
		return model.PriceTier{}, false
	}
	return tierAt(entry, at)
}

func pricingEntry(pricing model.PricingConfig, modelCode string) (model.ModelPricing, bool) {
	entry, ok := pricing.Models[modelCode]
	if !ok {
		entry, ok = pricing.Models["*"]
	}
	return entry, ok
}

func tierAt(entry model.ModelPricing, at time.Time) (model.PriceTier, bool) {
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

// PriceLevel classifies the price band a model is in right now.
type PriceLevel string

const (
	// PriceLevelFlat means no time-of-day price difference to act on.
	PriceLevelFlat   PriceLevel = ""
	PriceLevelValley PriceLevel = "valley"
	PriceLevelPeak   PriceLevel = "peak"
)

func tierRate(tier model.PriceTier) float64 {
	return tier.Input + tier.CachedInput + tier.Output
}

// PriceLevelAt compares the current tier with the cheapest tier of the same
// entry, so each model is judged against its own prices.
func PriceLevelAt(pricing model.PricingConfig, modelCode string, at time.Time) PriceLevel {
	entry, ok := pricingEntry(pricing, modelCode)
	if !ok || len(entry.Tiers) == 0 {
		return PriceLevelFlat
	}
	tier, ok := tierAt(entry, at)
	if !ok {
		return PriceLevelFlat
	}
	cheapest, priciest := math.Inf(1), math.Inf(-1)
	for _, candidate := range entry.Tiers {
		rate := tierRate(candidate)
		if rate < cheapest {
			cheapest = rate
		}
		if rate > priciest {
			priciest = rate
		}
	}
	if priciest-cheapest < 1e-9 {
		return PriceLevelFlat
	}
	if tierRate(tier) <= cheapest+1e-9 {
		return PriceLevelValley
	}
	return PriceLevelPeak
}

func priceRank(level PriceLevel) int {
	switch level {
	case PriceLevelValley:
		return 0
	case PriceLevelPeak:
		return 2
	}
	return 1
}

// OrderByTimePricing moves models currently in their valley band to the front
// and models in a pricier band to the back. Relative order inside each group
// is preserved, and models without a price difference stay in between.
func OrderByTimePricing(codes []string, pricing model.PricingConfig, at time.Time) []string {
	ordered := append([]string(nil), codes...)
	if len(ordered) < 2 {
		return ordered
	}
	ranks := make(map[string]int, len(ordered))
	banded := false
	for _, code := range ordered {
		level := PriceLevelAt(pricing, code, at)
		if level != PriceLevelFlat {
			banded = true
		}
		ranks[code] = priceRank(level)
	}
	if !banded {
		return ordered
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ranks[ordered[i]] < ranks[ordered[j]] })
	return ordered
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
