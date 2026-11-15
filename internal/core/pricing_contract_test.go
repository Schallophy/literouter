package core

import (
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/Schallophy/literouter/internal/model"
)

func contractPricing() model.PricingConfig {
	return model.PricingConfig{
		Currency: "CNY",
		Unit:     1_000_000,
		Models: map[string]model.ModelPricing{
			"model": {
				Timezone: "Asia/Shanghai",
				Tiers: []model.PriceTier{
					{From: "08:00", To: "22:00", Input: 2, CachedInput: 0.2, Output: 8},
					{From: "22:00", To: "08:00", Input: 1, CachedInput: 0.1, Output: 4},
				},
			},
		},
	}
}

func shanghaiAt(hour, minute int) time.Time {
	return time.Date(2026, 11, 11, hour-8, minute, 0, 0, time.UTC)
}

func TestPricingContract(t *testing.T) {
	pricing := contractPricing()
	tierAt := func(cfg model.PricingConfig, code string, at time.Time) (model.PriceTier, bool) {
		return PriceTierAt(cfg, code, at)
	}

	t.Run("picks the peak tier inside 08:00-22:00", func(t *testing.T) {
		if tier, ok := tierAt(pricing, "model", shanghaiAt(8, 0)); !ok || tier.Input != 2 {
			t.Fatalf("08:00: %+v ok=%v", tier, ok)
		}
		if tier, ok := tierAt(pricing, "model", shanghaiAt(21, 59)); !ok || tier.Input != 2 {
			t.Fatalf("21:59: %+v ok=%v", tier, ok)
		}
	})

	t.Run("picks the off-peak tier across midnight", func(t *testing.T) {
		for _, clock := range [][2]int{{22, 0}, {23, 59}, {0, 0}, {7, 59}} {
			if tier, ok := tierAt(pricing, "model", shanghaiAt(clock[0], clock[1])); !ok || tier.Input != 1 {
				t.Fatalf("%02d:%02d: %+v ok=%v", clock[0], clock[1], tier, ok)
			}
		}
	})

	t.Run("price tiers follow the configured timezone", func(t *testing.T) {
		instant := time.Date(2026, 11, 11, 0, 30, 0, 0, time.UTC)
		utc := contractPricing()
		entry := utc.Models["model"]
		entry.Timezone = "UTC"
		utc.Models["model"] = entry
		if tier, ok := tierAt(pricing, "model", instant); !ok || tier.Input != 2 {
			t.Fatalf("shanghai: %+v ok=%v", tier, ok)
		}
		if tier, ok := tierAt(utc, "model", instant); !ok || tier.Input != 1 {
			t.Fatalf("utc: %+v ok=%v", tier, ok)
		}
	})

	t.Run("price tiers follow daylight saving transitions", func(t *testing.T) {
		newYork := contractPricing()
		entry := newYork.Models["model"]
		entry.Timezone = "America/New_York"
		newYork.Models["model"] = entry
		if tier, ok := tierAt(newYork, "model", time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC)); !ok || tier.Input != 1 {
			t.Fatalf("06:30Z: %+v ok=%v", tier, ok)
		}
		if tier, ok := tierAt(newYork, "model", time.Date(2026, 3, 8, 12, 30, 0, 0, time.UTC)); !ok || tier.Input != 2 {
			t.Fatalf("12:30Z: %+v ok=%v", tier, ok)
		}
	})

	t.Run("an unknown model falls back to the wildcard entry", func(t *testing.T) {
		wildcard := model.PricingConfig{
			Currency: "CNY",
			Unit:     1_000_000,
			Models:   map[string]model.ModelPricing{"*": pricing.Models["model"]},
		}
		if tier, ok := tierAt(wildcard, "other", shanghaiAt(9, 0)); !ok || tier.Input != 2 {
			t.Fatalf("got %+v ok=%v", tier, ok)
		}
	})

	t.Run("no pricing or no matching window yields null", func(t *testing.T) {
		if _, ok := tierAt(model.PricingConfig{Currency: "CNY", Unit: 1_000_000, Models: map[string]model.ModelPricing{}}, "model", shanghaiAt(9, 0)); ok {
			t.Fatal("empty pricing should not resolve")
		}
		gapped := model.PricingConfig{
			Currency: "CNY",
			Unit:     1_000_000,
			Models: map[string]model.ModelPricing{"model": {
				Timezone: "Asia/Shanghai",
				Tiers:    []model.PriceTier{{From: "08:00", To: "22:00", Input: 2, CachedInput: 0.2, Output: 8}},
			}},
		}
		if _, ok := tierAt(gapped, "model", shanghaiAt(23, 0)); ok {
			t.Fatal("gap should not resolve")
		}
	})

	t.Run("a tier with identical from/to covers the whole day", func(t *testing.T) {
		allDay := model.PricingConfig{
			Currency: "CNY",
			Unit:     1_000_000,
			Models: map[string]model.ModelPricing{"model": {
				Timezone: "Asia/Shanghai",
				Tiers:    []model.PriceTier{{From: "00:00", To: "00:00", Input: 2, CachedInput: 0.2, Output: 8}},
			}},
		}
		for _, hour := range []int{3, 15} {
			if tier, ok := tierAt(allDay, "model", shanghaiAt(hour, 0)); !ok || tier.Output != 8 {
				t.Fatalf("%d:00: %+v ok=%v", hour, tier, ok)
			}
		}
	})

	usage := model.Usage{PromptTokens: 1_000, CompletionTokens: 100, TotalTokens: 1_100, CachedPromptTokens: 800, CacheReported: true}
	tier := model.PriceTier{From: "00:00", To: "00:00", Input: 2, CachedInput: 0.2, Output: 8}

	t.Run("cost splits fresh input, cache hits and output", func(t *testing.T) {
		if got := UsageCost(usage, tier, 1_000_000); got != 1_360.0/1_000_000 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("cost ignores impossible cache counts", func(t *testing.T) {
		impossible := model.Usage{PromptTokens: 100, CompletionTokens: 0, TotalTokens: 100, CachedPromptTokens: 500, CacheReported: true}
		clampTier := model.PriceTier{From: "00:00", To: "00:00", Input: 2, CachedInput: 1, Output: 8}
		if got := UsageCost(impossible, clampTier, 1_000_000); got != 100.0/1_000_000 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("cost parts split input, cache hits, output and savings", func(t *testing.T) {
		parts := UsageCostParts(usage, tier, 1_000_000)
		if parts.Input != (200*2)/1_000_000.0 {
			t.Fatalf("input %v", parts.Input)
		}
		if parts.CachedInput != (800*0.2)/1_000_000.0 {
			t.Fatalf("cached %v", parts.CachedInput)
		}
		if parts.Output != (100*8)/1_000_000.0 {
			t.Fatalf("output %v", parts.Output)
		}
		if parts.CacheSaved != (800*1.8)/1_000_000.0 {
			t.Fatalf("saved %v", parts.CacheSaved)
		}
		if parts.Input+parts.CachedInput+parts.Output != UsageCost(usage, tier, 1_000_000) {
			t.Fatalf("total mismatch: %+v", parts)
		}
	})

	t.Run("cost parts clamp cache counts and reject a non-positive unit", func(t *testing.T) {
		clampTier := model.PriceTier{From: "00:00", To: "00:00", Input: 2, CachedInput: 1, Output: 8}
		clamped := UsageCostParts(model.Usage{PromptTokens: 100, CachedPromptTokens: 500, CompletionTokens: 0}, clampTier, 1_000_000)
		if clamped.Input != 0 || clamped.CachedInput != 100.0/1_000_000 || clamped.CacheSaved != 100.0/1_000_000 {
			t.Fatalf("clamped %+v", clamped)
		}
		invalid := UsageCostParts(model.Usage{PromptTokens: 100, CachedPromptTokens: 50, CompletionTokens: 10}, clampTier, 0)
		if invalid != (model.CostSplit{}) {
			t.Fatalf("invalid %+v", invalid)
		}
	})
}
