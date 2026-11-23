package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Schallophy/literouter/internal/model"
)

func toMap(t *testing.T, cfg model.GatewayConfig) map[string]any {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return normalizeJSONNumbers(value).(map[string]any)
}

func hasSubstring(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func TestDefaultsValidateCleanly(t *testing.T) {
	result := Normalize(toMap(t, DefaultConfig()))
	if !result.OK || len(result.Errors) != 0 {
		t.Fatalf("expected clean defaults, got %v", result.Errors)
	}
	if len(result.Config.Chain.Models) != 0 {
		t.Fatalf("expected empty chain, got %v", result.Config.Chain.Models)
	}
}

func TestLegacyActiveMovedToFront(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["chain"].(map[string]any)["models"] = []any{"dashscope/a", "dashscope/b", "dashscope/c"}
	raw["chain"].(map[string]any)["active"] = "dashscope/b"
	result := Normalize(raw)
	if !result.OK {
		t.Fatalf("unexpected errors %v", result.Errors)
	}
	got := result.Config.Chain.Models
	want := []string{"dashscope/b", "dashscope/a", "dashscope/c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if !hasSubstring(result.Migrations, "chain.active 已废弃") || !hasSubstring(result.Migrations, "dashscope/b") {
		t.Fatalf("missing migration: %v", result.Migrations)
	}
}

func TestLegacyActiveOutsideListWarns(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["chain"].(map[string]any)["models"] = []any{"dashscope/a", "dashscope/b"}
	raw["chain"].(map[string]any)["active"] = "dashscope/c"
	result := Normalize(raw)
	if !result.OK {
		t.Fatalf("unexpected errors %v", result.Errors)
	}
	if !hasSubstring(result.Warnings, "chain.active") {
		t.Fatalf("missing warning: %v", result.Warnings)
	}
}

func TestInvalidPortAndURLRejected(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Port = 99999
	cfg.Providers[0].BaseURL = "ftp://example.com"
	result := Normalize(toMap(t, cfg))
	if result.OK || len(result.Errors) != 2 {
		t.Fatalf("expected two errors, got %v", result.Errors)
	}
}

func TestEmptyChainWarns(t *testing.T) {
	result := Normalize(toMap(t, DefaultConfig()))
	if result.OK != true || !hasSubstring(result.Warnings, "还没有任何模型") {
		t.Fatalf("missing warning: %v", result.Warnings)
	}
}

func TestDuplicateModelsDeduped(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["chain"].(map[string]any)["models"] = []any{"dashscope/a", "dashscope/a", "dashscope/b"}
	got := Normalize(raw).Config.Chain.Models
	if strings.Join(got, ",") != "dashscope/a,dashscope/b" {
		t.Fatalf("got %v", got)
	}
}

func TestLegacyChainsMigrate(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	delete(raw, "chain")
	raw["chains"] = []any{
		map[string]any{"name": "text", "purpose": "tools", "models": []any{"dashscope/a"}, "context": map[string]any{"window": 64000, "onOverflow": "passthrough"}},
		map[string]any{"name": "main", "purpose": "default", "models": []any{"dashscope/b", "dashscope/c"}, "context": map[string]any{"window": 128000, "onOverflow": "reject"}},
	}
	result := Normalize(raw)
	if !result.OK {
		t.Fatalf("unexpected errors %v", result.Errors)
	}
	if strings.Join(result.Config.Chain.Models, ",") != "dashscope/b,dashscope/c" {
		t.Fatalf("got %v", result.Config.Chain.Models)
	}
	if result.Config.Chain.Context.Window != 128000 || result.Config.Chain.Context.OnOverflow != model.OverflowReject {
		t.Fatalf("context not migrated: %+v", result.Config.Chain.Context)
	}
	if !hasSubstring(result.Migrations, "合并") {
		t.Fatalf("missing migration: %v", result.Migrations)
	}
}

func TestContextDefaultsAndOverrides(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	delete(raw["chain"].(map[string]any), "context")
	result := Normalize(raw)
	if result.Config.Chain.Context.Window != 256000 || result.Config.Chain.Context.OnOverflow != model.OverflowReject {
		t.Fatalf("bad defaults: %+v", result.Config.Chain.Context)
	}
	raw["chain"].(map[string]any)["context"] = map[string]any{"window": 64000, "onOverflow": "passthrough"}
	overridden := Normalize(raw)
	if overridden.Config.Chain.Context.Window != 64000 || overridden.Config.Chain.Context.OnOverflow != model.OverflowPassthrough {
		t.Fatalf("bad override: %+v", overridden.Config.Chain.Context)
	}
}

func TestRetryDeadDefaultAndBounds(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	delete(raw["chain"].(map[string]any), "retryDeadAfterMinutes")
	if got := Normalize(raw).Config.Chain.RetryDeadAfterMinutes; got != 1440 {
		t.Fatalf("got %d", got)
	}
	raw["chain"].(map[string]any)["retryDeadAfterMinutes"] = 0
	if got := Normalize(raw).Config.Chain.RetryDeadAfterMinutes; got != 0 {
		t.Fatalf("got %d", got)
	}
	raw["chain"].(map[string]any)["retryDeadAfterMinutes"] = 10080
	if got := Normalize(raw).Config.Chain.RetryDeadAfterMinutes; got != 10080 {
		t.Fatalf("got %d", got)
	}
	raw["chain"].(map[string]any)["retryDeadAfterMinutes"] = -1
	if Normalize(raw).OK {
		t.Fatal("expected failure")
	}
}

func TestInvalidContextRejected(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["chain"].(map[string]any)["context"] = map[string]any{"window": 999, "onOverflow": "truncate"}
	result := Normalize(raw)
	if result.OK || !hasSubstring(result.Errors, "context.window") || !hasSubstring(result.Errors, "onOverflow") {
		t.Fatalf("got %v", result.Errors)
	}
	raw["chain"].(map[string]any)["context"] = "256k"
	if Normalize(raw).OK == true {
		t.Fatal("expected non-object context to fail")
	}
}

func TestModelApisDefaultsAndCanonicalOrder(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["chain"].(map[string]any)["models"] = []any{"dashscope/a", "dashscope/b"}
	delete(raw, "modelApis")
	result := Normalize(raw)
	if len(result.Config.ModelApis["dashscope/a"]) != 1 || result.Config.ModelApis["dashscope/a"][0] != model.ApiChat {
		t.Fatalf("bad default: %v", result.Config.ModelApis)
	}

	raw2 := toMap(t, DefaultConfig())
	raw2["modelApis"] = map[string]any{"dashscope/kimi": []any{"responses", "chat", "responses"}}
	got := Normalize(raw2).Config.ModelApis["dashscope/kimi"]
	if len(got) != 2 || got[0] != model.ApiChat || got[1] != model.ApiResponses {
		t.Fatalf("bad order: %v", got)
	}
}

func TestModelApisUnknownAndEmptyRejected(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["chain"].(map[string]any)["models"] = []any{"dashscope/a", "dashscope/b"}
	raw["modelApis"] = map[string]any{
		"dashscope/a": []any{"telepathy"},
		"dashscope/b": []any{},
	}
	result := Normalize(raw)
	if result.OK || !hasSubstring(result.Errors, "telepathy") || !hasSubstring(result.Errors, "dashscope/b") {
		t.Fatalf("got %v", result.Errors)
	}
	if result.Config.ModelApis["dashscope/b"][0] != model.ApiChat {
		t.Fatalf("fallback missing: %v", result.Config.ModelApis)
	}
}

func TestBlocksFallBackToDefaults(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	delete(raw, "ui")
	delete(raw, "server")
	delete(raw, "log")
	result := Normalize(raw)
	if !result.Config.UI.Enabled || result.Config.Server.MaxBodyBytes != 16*1024*1024 || !result.Config.Server.Cors {
		t.Fatalf("bad server/ui defaults: %+v %+v", result.Config.UI, result.Config.Server)
	}
	logCfg := result.Config.Log
	if logCfg.Level != model.LogInfo || logCfg.File != "" || logCfg.MaxBytes != 2*1024*1024 || logCfg.MaxFiles != 3 {
		t.Fatalf("bad log defaults: %+v", logCfg)
	}
}

func TestInvalidServerUILogRejected(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["server"] = map[string]any{"maxBodyBytes": 12}
	raw["ui"] = map[string]any{"enabled": "yes"}
	raw["log"] = map[string]any{"level": "verbose"}
	result := Normalize(raw)
	if result.OK || !hasSubstring(result.Errors, "server.maxBodyBytes") || !hasSubstring(result.Errors, "log.level") {
		t.Fatalf("got %v", result.Errors)
	}
	if !result.Config.UI.Enabled {
		t.Fatal("ui.enabled should fall back to true")
	}
}

func TestLegacySingleUpstreamMigrated(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	delete(raw, "providers")
	delete(raw, "defaultProvider")
	raw["upstream"] = map[string]any{
		"baseUrl":           "https://api.example.com/v1",
		"apiKeyEnv":         "EXAMPLE_KEY",
		"apiKey":            "sk-inline",
		"requestTimeoutMs":  5000,
		"injectStreamUsage": false,
	}
	result := Normalize(raw)
	if !result.OK {
		t.Fatalf("unexpected errors %v", result.Errors)
	}
	if !hasSubstring(result.Migrations, "迁移") {
		t.Fatalf("missing migration: %v", result.Migrations)
	}
	if len(result.Config.Providers) != 1 || result.Config.Providers[0].ID != "default" ||
		result.Config.Providers[0].BaseURL != "https://api.example.com/v1" ||
		result.Config.Providers[0].APIKey != "sk-inline" ||
		result.Config.DefaultProvider != "default" {
		t.Fatalf("bad migration: %+v", result.Config.Providers)
	}
}

func TestMultipleProvidersKeepOrder(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["providers"] = []any{
		map[string]any{"id": "dashscope", "baseUrl": "https://dashscope.example/v1", "apiKeyEnv": "A"},
		map[string]any{"id": "openai", "label": "OpenAI", "baseUrl": "https://api.openai.com/v1", "apiKeyEnv": "B"},
	}
	raw["defaultProvider"] = "openai"
	result := Normalize(raw)
	if !result.OK {
		t.Fatalf("unexpected errors %v", result.Errors)
	}
	if result.Config.Providers[0].ID != "dashscope" || result.Config.Providers[1].ID != "openai" ||
		result.Config.Providers[1].Label != "OpenAI" || result.Config.DefaultProvider != "openai" {
		t.Fatalf("bad providers: %+v", result.Config.Providers)
	}
}

func TestDuplicateOrMalformedProviderIDs(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["providers"] = []any{
		map[string]any{"id": "a", "baseUrl": "https://a.example/v1"},
		map[string]any{"id": "a", "baseUrl": "https://b.example/v1"},
		map[string]any{"id": "Bad Id", "baseUrl": "https://c.example/v1"},
	}
	result := Normalize(raw)
	if result.OK || !hasSubstring(result.Errors, "重复") || !hasSubstring(result.Errors, "id 只能使用") {
		t.Fatalf("got %v", result.Errors)
	}
}

func TestUnknownDefaultProviderFallsBack(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["defaultProvider"] = "nope"
	result := Normalize(raw)
	if result.Config.DefaultProvider != result.Config.Providers[0].ID || !hasSubstring(result.Warnings, "defaultProvider") {
		t.Fatalf("bad fallback: %+v %v", result.Config.DefaultProvider, result.Warnings)
	}
}

func TestLegacyModelProvidersBinding(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["chain"] = map[string]any{"active": "qwen3.8-27b", "models": []any{"qwen3.8-27b"}}
	raw["modelProviders"] = map[string]any{"qwen3.8-27b": "dashscope"}
	result := Normalize(raw)
	if !result.OK || strings.Join(result.Config.Chain.Models, ",") != "dashscope/qwen3.8-27b" {
		t.Fatalf("got %v %v", result.Config.Chain.Models, result.Errors)
	}
	if !hasSubstring(result.Migrations, "modelProviders") {
		t.Fatalf("missing migration: %v", result.Migrations)
	}

	raw["modelProviders"] = map[string]any{"qwen3.8-27b": "nowhere"}
	if bad := Normalize(raw); bad.OK || !hasSubstring(bad.Errors, "不存在的提供商") {
		t.Fatalf("expected binding error, got %v", bad.Errors)
	}
}

func TestLegacyModelUpstreamsBinding(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["chain"] = map[string]any{"active": "qwen3.8-27b", "models": []any{"qwen3.8-27b"}}
	raw["modelUpstreams"] = map[string]any{"qwen3.8-27b": "dashscope"}
	result := Normalize(raw)
	if !result.OK || !hasSubstring(result.Migrations, "modelUpstreams") {
		t.Fatalf("got %v %v", result.Migrations, result.Errors)
	}
	if strings.Join(result.Config.Chain.Models, ",") != "dashscope/qwen3.8-27b" {
		t.Fatalf("got %v", result.Config.Chain.Models)
	}
}

func TestSameUpstreamOnTwoProvidersStaysTwoModels(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["providers"] = append(raw["providers"].([]any), map[string]any{"id": "alpha", "baseUrl": "https://alpha.example/v1"})
	raw["chain"].(map[string]any)["models"] = []any{"dashscope/qwen3.8-max", "alpha/qwen3.8-max"}
	result := Normalize(raw)
	if !result.OK {
		t.Fatalf("unexpected errors %v", result.Errors)
	}
	if len(result.Config.ModelApis) != 2 {
		t.Fatalf("expected two model apis, got %v", result.Config.ModelApis)
	}
	raw["pricing"] = map[string]any{"models": map[string]any{
		"dashscope/qwen3.8-max": map[string]any{"tiers": []any{map[string]any{"from": "00:00", "to": "00:00", "input": 1, "cachedInput": 0.1, "output": 4}}},
		"alpha/qwen3.8-max":     map[string]any{"tiers": []any{map[string]any{"from": "00:00", "to": "00:00", "input": 2, "cachedInput": 0.2, "output": 8}}},
	}}
	priced := Normalize(raw)
	if !priced.OK {
		t.Fatalf("unexpected errors %v", priced.Errors)
	}
	if priced.Config.Pricing.Models["dashscope/qwen3.8-max"].Tiers[0].Input != 1 || priced.Config.Pricing.Models["alpha/qwen3.8-max"].Tiers[0].Input != 2 {
		t.Fatalf("bad pricing: %+v", priced.Config.Pricing.Models)
	}
}

func TestLegacyUpstreamsArrayMigrated(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	providers := raw["providers"]
	delete(raw, "providers")
	delete(raw, "defaultProvider")
	raw["upstreams"] = providers
	raw["defaultUpstream"] = "dashscope"
	result := Normalize(raw)
	if !result.OK || !hasSubstring(result.Migrations, "upstreams") {
		t.Fatalf("got %v %v", result.Migrations, result.Errors)
	}
	if len(result.Config.Providers) != 1 || result.Config.DefaultProvider != "dashscope" {
		t.Fatalf("bad migration: %+v", result.Config.Providers)
	}
}

func TestPricingDefaults(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	delete(raw, "pricing")
	result := Normalize(raw)
	if result.Config.Pricing.Currency != "CNY" || result.Config.Pricing.Unit != 1_000_000 || len(result.Config.Pricing.Models) != 0 {
		t.Fatalf("bad defaults: %+v", result.Config.Pricing)
	}
	if !result.Config.Pricing.PreferOffPeak {
		t.Fatal("preferOffPeak should default to true")
	}
}

func TestPreferOffPeakCanBeDisabled(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["pricing"].(map[string]any)["preferOffPeak"] = false
	result := Normalize(raw)
	if !result.OK || result.Config.Pricing.PreferOffPeak {
		t.Fatalf("got %+v %v", result.Config.Pricing, result.Errors)
	}
}

func TestPricingTimezonePerModel(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["pricing"] = map[string]any{"models": map[string]any{
		"dashscope/a": map[string]any{"timezone": "Asia/Tokyo", "tiers": []any{map[string]any{"from": "00:00", "to": "00:00", "input": 1, "cachedInput": 0.1, "output": 4}}},
	}}
	result := Normalize(raw)
	if !result.OK || result.Config.Pricing.Models["dashscope/a"].Timezone != "Asia/Tokyo" {
		t.Fatalf("got %+v %v", result.Config.Pricing.Models, result.Errors)
	}
	if hasSubstring(result.Migrations, "补充时区") {
		t.Fatalf("unexpected migration: %v", result.Migrations)
	}
	raw["pricing"].(map[string]any)["models"].(map[string]any)["dashscope/a"].(map[string]any)["timezone"] = "Mars/Olympus"
	if failed := Normalize(raw); failed.OK || !hasSubstring(failed.Errors, "timezone") {
		t.Fatalf("expected timezone failure, got %v", failed.Errors)
	}
}

func TestPricingMissingTimezoneMigrated(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	raw["pricing"] = map[string]any{"models": map[string]any{
		"dashscope/a": map[string]any{"tiers": []any{map[string]any{"from": "00:00", "to": "00:00", "input": 1, "cachedInput": 0.1, "output": 4}}},
	}}
	result := Normalize(raw)
	if result.Config.Pricing.Models["dashscope/a"].Timezone != SystemTimeZone() || !hasSubstring(result.Migrations, "补充时区") {
		t.Fatalf("got %+v %v", result.Config.Pricing.Models, result.Migrations)
	}
}

func TestPricingTierValidation(t *testing.T) {
	base := func(tier map[string]any) map[string]any {
		raw := toMap(t, DefaultConfig())
		raw["pricing"] = map[string]any{"models": map[string]any{"dashscope/m": map[string]any{"tiers": []any{tier}}}}
		return raw
	}

	ordered := Normalize(base(map[string]any{"from": "22:00", "to": "08:00", "input": 1, "cachedInput": 0.1, "output": 4}))
	if ordered.OK {
		t.Fatal("single non-covering tier must fail")
	}

	ok := Normalize(base(map[string]any{"from": "00:00", "to": "00:00", "input": 2, "cachedInput": 0.2, "output": 8}))
	if !ok.OK || ok.Config.Pricing.Models["dashscope/m"].Tiers[0].Input != 2 {
		t.Fatalf("got %v %v", ok.Errors, ok.Config.Pricing.Models)
	}

	cached := Normalize(base(map[string]any{"from": "00:00", "to": "00:00", "input": 1, "cachedInput": 2, "output": 4}))
	if cached.OK || !hasSubstring(cached.Errors, "cachedInput") {
		t.Fatalf("got %v", cached.Errors)
	}

	clock := Normalize(base(map[string]any{"from": "25:00", "to": "08:00", "input": 1, "cachedInput": 0.1, "output": 4}))
	if clock.OK || !hasSubstring(clock.Errors, "HH:MM") {
		t.Fatalf("got %v", clock.Errors)
	}
}

func TestResponsesChainingValidation(t *testing.T) {
	raw := toMap(t, DefaultConfig())
	delete(raw, "responsesChaining")
	if got := Normalize(raw).Config.ResponsesChaining; got.Enabled || got.TTLMinutes != 120 || got.MaxSessions != 500 {
		t.Fatalf("bad defaults: %+v", got)
	}
	raw["responsesChaining"] = map[string]any{"enabled": true, "ttlMinutes": 30, "maxSessions": 42}
	enabled := Normalize(raw)
	if enabled.Config.ResponsesChaining != (model.ResponsesChainingConfig{Enabled: true, TTLMinutes: 30, MaxSessions: 42}) {
		t.Fatalf("bad enabled: %+v", enabled.Config.ResponsesChaining)
	}
	raw["responsesChaining"] = map[string]any{"enabled": true, "ttlMinutes": 0, "maxSessions": 0}
	broken := Normalize(raw)
	if broken.OK || !hasSubstring(broken.Errors, "responsesChaining.ttlMinutes") || !hasSubstring(broken.Errors, "responsesChaining.maxSessions") {
		t.Fatalf("got %v", broken.Errors)
	}
}

func TestLogRotationBounds(t *testing.T) {
	if got := Normalize(toMap(t, DefaultConfig())).Config.Log; got.MaxBytes != 2*1024*1024 || got.MaxFiles != 3 {
		t.Fatalf("bad defaults: %+v", got)
	}
	raw := toMap(t, DefaultConfig())
	raw["log"] = map[string]any{"maxBytes": -1, "maxFiles": 999}
	if Normalize(raw).OK {
		t.Fatal("expected failure")
	}
}

func TestJSONCToleratesCommentsAndTrailingCommas(t *testing.T) {
	text := `{
    // 行注释
    "version": 1,
    /* 块注释 */
    "port": 8788,
    "chain": {
      "active": "a",
      "models": ["a", "b",],
    },
  }`
	parsed, err := ParseConfigText(text)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	obj := parsed.(map[string]any)
	if obj["port"] != 8788.0 {
		t.Fatalf("port = %#v", obj["port"])
	}
	models := obj["chain"].(map[string]any)["models"].([]any)
	if len(models) != 2 || models[0] != "a" || models[1] != "b" {
		t.Fatalf("models = %#v", models)
	}
}

func TestJSONCStringContentsPreserved(t *testing.T) {
	parsed, err := ParseConfigText(`{"baseUrl":"http://x//y","note":"/* keep */","q":"a,]"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	obj := parsed.(map[string]any)
	if obj["baseUrl"] != "http://x//y" || obj["note"] != "/* keep */" || obj["q"] != "a,]" {
		t.Fatalf("got %#v", obj)
	}
	// A URL inside a commented config must survive comment stripping.
	parsed, err = ParseConfigText("{\n // 注释\n \"baseUrl\": \"https://api.example.com//v1\"\n}")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.(map[string]any)["baseUrl"] != "https://api.example.com//v1" {
		t.Fatalf("url mangled: %#v", parsed)
	}
}

func TestBrokenJSONFails(t *testing.T) {
	for _, text := range []string{"{ nope", `{"a": 1,, }`} {
		if _, err := ParseConfigText(text); err == nil {
			t.Fatalf("expected error for %q", text)
		}
	}
}

func TestLoadCommentedConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := SaveConfig(file, DefaultConfig()); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	commented := strings.Replace(string(raw), `"version": 1,`, "\"version\": 1, // hand written\n  \"note\": \"ignored\" ,", 1)
	if err := os.WriteFile(file, []byte(commented), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := LoadConfig(file)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Created || loaded.Config.Port != 8788 || len(loaded.Config.Chain.Models) != 0 {
		t.Fatalf("bad load: %+v", loaded)
	}
}

func TestSaveIsStrictJSONAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	cfg := DefaultConfig()
	cfg.Chain.Models = []string{"dashscope/a", "dashscope/b"}
	cfg.Chain.Context = model.ContextConfig{Window: 64000, OnOverflow: model.OverflowPassthrough}
	cfg.Server.MaxBodyBytes = 1048576
	cfg.Log.Level = model.LogDebug
	if err := SaveConfig(file, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("strict json: %v", err)
	}
	chain := parsed["chain"].(map[string]any)
	if _, ok := chain["active"]; ok {
		t.Fatal("active must not be persisted")
	}
	loaded, err := LoadConfig(file)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if strings.Join(loaded.Config.Chain.Models, ",") != "dashscope/a,dashscope/b" ||
		loaded.Config.Chain.Context.Window != 64000 ||
		loaded.Config.Chain.Context.OnOverflow != model.OverflowPassthrough ||
		loaded.Config.Server.MaxBodyBytes != 1048576 ||
		loaded.Config.Log.Level != model.LogDebug {
		t.Fatalf("round trip mismatch: %+v", loaded.Config)
	}
}

func TestStrictLegacyConfigMigratedAndWrittenBack(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	raw := toMap(t, DefaultConfig())
	raw["chain"] = map[string]any{"active": "qwen3.8-27b", "models": []any{"qwen3.8-27b"}}
	raw["modelProviders"] = map[string]any{"qwen3.8-27b": "dashscope"}
	data, _ := json.MarshalIndent(raw, "", "  ")
	if err := os.WriteFile(file, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := LoadConfig(file)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.Persisted || strings.Join(loaded.Config.Chain.Models, ",") != "dashscope/qwen3.8-27b" {
		t.Fatalf("bad migration: %+v", loaded)
	}
	var saved map[string]any
	savedRaw, _ := os.ReadFile(file)
	if err := json.Unmarshal(savedRaw, &saved); err != nil {
		t.Fatalf("saved strict json: %v", err)
	}
	if _, ok := saved["modelProviders"]; ok {
		t.Fatal("modelProviders must not be persisted")
	}
	again, err := LoadConfig(file)
	if err != nil {
		t.Fatalf("load again: %v", err)
	}
	if again.Persisted || len(again.Migrations) != 0 {
		t.Fatalf("second load should not migrate: %+v", again)
	}
}

func TestCommentedLegacyConfigNotRewritten(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	text := `{
  // 手写注释不能丢
  "providers": [{ "id": "dashscope", "baseUrl": "https://example.com/v1" }],
  "chain": { "models": ["qwen3.8-27b"] },
  "modelProviders": { "qwen3.8-27b": "dashscope" },
}
`
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := LoadConfig(file)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Persisted || len(loaded.Migrations) == 0 {
		t.Fatalf("should report without persisting: %+v", loaded)
	}
	if strings.Join(loaded.Config.Chain.Models, ",") != "dashscope/qwen3.8-27b" {
		t.Fatalf("got %v", loaded.Config.Chain.Models)
	}
	after, _ := os.ReadFile(file)
	if string(after) != text {
		t.Fatal("commented file must not be rewritten")
	}
}

func TestLoadCreatesDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	loaded, err := LoadConfig(file)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.Created || loaded.Config.Port != 8788 {
		t.Fatalf("bad creation: %+v", loaded)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("config not created: %v", err)
	}
}

func TestInvalidConfigReturnsDetails(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte(`{"port": 99999}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadConfig(file)
	configErr, ok := err.(*ConfigError)
	if !ok || len(configErr.Details) == 0 {
		t.Fatalf("expected ConfigError with details, got %v", err)
	}
}

func TestShortPathDirectoryLoad(t *testing.T) {
	base := os.TempDir()
	dir, err := os.MkdirTemp(base, "LR8~1")
	if err != nil {
		t.Skipf("cannot create short dir: %v", err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "config.json")
	if _, err := LoadConfig(file); err != nil {
		t.Fatalf("load in short path: %v", err)
	}
}
