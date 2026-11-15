package core

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Schallophy/literouter/internal/model"
)

func intp(value int) *int           { return &value }
func strp(value string) *string     { return &value }
func floatp(value float64) *float64 { return &value }

func testPricing() model.PricingConfig {
	return model.PricingConfig{
		Currency: "CNY",
		Unit:     1_000_000,
		Models: map[string]model.ModelPricing{
			"dashscope/a": {
				Timezone: "Asia/Shanghai",
				Tiers: []model.PriceTier{
					{From: "22:00", To: "08:00", Input: 1, CachedInput: 0.1, Output: 4},
					{From: "08:00", To: "22:00", Input: 2, CachedInput: 0.2, Output: 8},
				},
			},
			"*": {
				Timezone: "UTC",
				Tiers:    []model.PriceTier{{From: "00:00", To: "00:00", Input: 3, CachedInput: 0.3, Output: 6}},
			},
		},
	}
}

func TestPriceTierUsesModelTimezone(t *testing.T) {
	pricing := testPricing()
	// 2026-01-15 15:00 UTC == 23:00 Asia/Shanghai (night tier).
	at := time.Date(2026, 1, 15, 15, 0, 0, 0, time.UTC)
	tier, ok := PriceTierAt(pricing, "dashscope/a", at)
	if !ok || tier.Input != 1 {
		t.Fatalf("expected night tier, got %+v ok=%v", tier, ok)
	}
	// 2026-01-15 04:00 UTC == 12:00 Asia/Shanghai (day tier).
	day := time.Date(2026, 1, 15, 4, 0, 0, 0, time.UTC)
	tier, ok = PriceTierAt(pricing, "dashscope/a", day)
	if !ok || tier.Input != 2 {
		t.Fatalf("expected day tier, got %+v ok=%v", tier, ok)
	}
}

func TestPriceTierFallbackAndBoundaries(t *testing.T) {
	pricing := testPricing()
	fallback, ok := PriceTierAt(pricing, "unknown/model", time.Now())
	if !ok || fallback.Input != 3 {
		t.Fatalf("expected wildcard fallback, got %+v", fallback)
	}
	// Exactly 08:00 local belongs to the day tier.
	at := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC) // 08:00 Shanghai
	if tier, _ := PriceTierAt(pricing, "dashscope/a", at); tier.Input != 2 {
		t.Fatalf("08:00 should be day tier, got %+v", tier)
	}
	// Exactly 22:00 local belongs to the night tier.
	night := time.Date(2026, 1, 15, 14, 0, 0, 0, time.UTC) // 22:00 Shanghai
	if tier, _ := PriceTierAt(pricing, "dashscope/a", night); tier.Input != 1 {
		t.Fatalf("22:00 should be night tier, got %+v", tier)
	}
}

func TestPriceTierWithoutEntry(t *testing.T) {
	if _, ok := PriceTierAt(model.PricingConfig{}, "any", time.Now()); ok {
		t.Fatal("empty pricing must not match")
	}
}

func TestUsageCostPartsSplitsCache(t *testing.T) {
	usage := model.Usage{PromptTokens: 1000, CachedPromptTokens: 800, CompletionTokens: 100, CacheReported: true}
	tier := model.PriceTier{Input: 2, CachedInput: 0.2, Output: 8}
	parts := UsageCostParts(usage, tier, 1_000_000)
	if parts.Input != 400.0/1_000_000 || parts.CachedInput != 160.0/1_000_000 || parts.Output != 800.0/1_000_000 {
		t.Fatalf("bad parts: %+v", parts)
	}
	if parts.CacheSaved != 1440.0/1_000_000 {
		t.Fatalf("bad cache saved: %v", parts.CacheSaved)
	}
	if UsageCost(usage, tier, 1_000_000) != 1360.0/1_000_000 {
		t.Fatalf("bad total: %v", UsageCost(usage, tier, 1_000_000))
	}
	if got := UsageCostParts(usage, tier, 0); got != (model.CostSplit{}) {
		t.Fatalf("zero unit must be free: %+v", got)
	}
}

func TestUsageCostClampsCached(t *testing.T) {
	usage := model.Usage{PromptTokens: 100, CachedPromptTokens: 500, CompletionTokens: -5}
	parts := UsageCostParts(usage, model.PriceTier{Input: 1, CachedInput: 0.5, Output: 1}, 1000)
	if parts.Input != 0 || parts.CachedInput != 100*0.5/1000 || parts.Output != 0 {
		t.Fatalf("bad clamp: %+v", parts)
	}
}

func TestClassifyHTTP(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		msg    string
		kind   model.FailureKind
		fatal  bool
		dead   bool
		inv    bool
	}{
		{"429 throttles", 429, "", "", model.FailureThrottled, false, false, false},
		{"rate limit text", 400, "", "rate limit reached", model.FailureThrottled, false, false, false},
		{"free tier quota", 403, "FreeTierOnly", "", model.FailureQuotaExhausted, false, true, false},
		{"allocation quota", 403, "AllocationQuota.FreeTierOnly", "", model.FailureQuotaExhausted, false, true, false},
		{"free quota text", 403, "", "free quota exhausted", model.FailureQuotaExhausted, false, true, false},
		{"401 auth", 401, "", "", model.FailureAuth, true, false, false},
		{"model missing", 404, "ModelNotFound", "", model.FailureModelInvalid, false, false, true},
		{"model not exist text", 400, "", "model x does not exist", model.FailureModelInvalid, false, false, true},
		{"403 forbidden", 403, "", "AccessDenied", model.FailureForbidden, false, false, false},
		{"403 not activated", 403, "", "model not activated", model.FailureForbidden, false, false, false},
		{"404 provider", 404, "", "", model.FailureProvider, false, false, false},
		{"400 bad request", 400, "", "", model.FailureBadRequest, false, false, false},
		{"422 bad request", 422, "", "", model.FailureBadRequest, false, false, false},
		{"500 provider", 500, "", "", model.FailureProvider, false, false, false},
		{"unknown", 418, "", "", model.FailureUnknown, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := ErrorPayload{Message: tc.msg}
			if tc.code != "" {
				payload.Code = &tc.code
			}
			got := ClassifyHTTPError(tc.status, payload)
			if got.Kind != tc.kind || got.Fatal != tc.fatal || got.MarkDead != tc.dead || got.MarkInvalid != tc.inv {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestParseErrorPayload(t *testing.T) {
	cases := []struct {
		raw     string
		code    string
		message string
	}{
		{`{"error":{"code":"X","message":"boom"}}`, "X", "boom"},
		{`{"error":{"type":"T","message":"m"}}`, "T", "m"},
		{`{"error":"plain"}`, "", "plain"},
		{`{"code":"C","msg":"m2"}`, "C", "m2"},
		{`{"detail":"d"}`, "", "d"},
		{`not json`, "", "not json"},
		{`{"error":{"message":"multi\nline   text"}}`, "", "multi line text"},
	}
	for _, tc := range cases {
		got := ParseErrorPayload(tc.raw)
		code := ""
		if got.Code != nil {
			code = *got.Code
		}
		if code != tc.code || got.Message != tc.message {
			t.Fatalf("%q -> %+v", tc.raw, got)
		}
	}
}

func TestClassifyNetwork(t *testing.T) {
	if got := ClassifyNetworkError(nil, false, true); got.Kind != model.FailureAborted || !got.Fatal || got.Retryable {
		t.Fatalf("bad aborted: %+v", got)
	}
	if got := ClassifyNetworkError(nil, true, false); got.Kind != model.FailureTimeout || !got.Retryable {
		t.Fatalf("bad timeout: %+v", got)
	}
	if got := ClassifyNetworkError(context.DeadlineExceeded, false, false); got.Kind != model.FailureNetwork || got.Message == "" {
		t.Fatalf("bad network: %+v", got)
	}
}

func TestCleanMessage(t *testing.T) {
	long := strings.Repeat("错", 700)
	if got := CleanMessage(" a \n\t b "); got != "a b" {
		t.Fatalf("got %q", got)
	}
	if got := CleanMessage(long); len([]rune(got)) != 600 {
		t.Fatalf("len = %d", len([]rune(got)))
	}
}

func TestSplitModelCodeLongestPrefix(t *testing.T) {
	providers := []model.ProviderConfig{{ID: "open"}, {ID: "openrouter"}, {ID: "a"}}
	parsed, ok := SplitModelCode("openrouter/nvidia/model", providers)
	if !ok || parsed.Provider != "openrouter" || parsed.Upstream != "nvidia/model" {
		t.Fatalf("got %+v ok=%v", parsed, ok)
	}
	if JoinModelCode("p", "/m") != "p/m" {
		t.Fatal("join must drop leading slash")
	}
	for _, bad := range []string{"", "nope/m", "openrouter/", "  "} {
		if _, ok := SplitModelCode(bad, providers); ok {
			t.Fatalf("%q should not parse", bad)
		}
	}
}

func TestRouteResolution(t *testing.T) {
	chain := model.ChainConfig{Models: []string{"p/a", "p/b", "q/b"}}
	providers := []model.ProviderConfig{{ID: "p"}, {ID: "q"}}
	if got := ResolveRoute("", chain, providers); got.Mode != RouteAuto || got.Model != "p/a" {
		t.Fatalf("got %+v", got)
	}
	if got := ResolveRoute("auto", chain, providers); got.Mode != RouteAuto || got.Model != "p/a" {
		t.Fatalf("got %+v", got)
	}
	if got := ResolveRoute("p/b", chain, providers); got.Mode != RouteDirect || got.Model != "p/b" {
		t.Fatalf("got %+v", got)
	}
	// a is unique by upstream name
	if got := ResolveRoute("a", chain, providers); got.Mode != RouteDirect || got.Model != "p/a" {
		t.Fatalf("got %+v", got)
	}
	// b exists twice, so a bare name must pass through
	if got := ResolveRoute("b", chain, providers); got.Mode != RoutePassthrough || got.Model != "b" {
		t.Fatalf("got %+v", got)
	}
	if got := ResolveRoute("unknown", chain, providers); got.Mode != RoutePassthrough || got.Model != "unknown" {
		t.Fatalf("got %+v", got)
	}
}

func rec(code string, health model.Health, checked *string, throttled *string) *model.ModelRecord {
	return &model.ModelRecord{Code: code, Health: health, CheckedAt: checked, ThrottledUntil: throttled}
}

func TestSelectCandidatesFilters(t *testing.T) {
	chain := model.ChainConfig{Models: []string{"a", "b", "c", "d", "e"}}
	records := map[string]*model.ModelRecord{
		"a": rec("a", model.HealthAlive, nil, nil),
		"b": rec("b", model.HealthInvalid, nil, nil),
		"c": rec("c", model.HealthDead, strp("2026-01-01T00:00:00.000Z"), nil),
		"d": rec("d", model.HealthAlive, nil, strp("2099-01-01T00:00:00.000Z")),
	}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).UnixMilli()
	got := SelectCandidates(SelectionInput{
		Chain: chain,
		GetRecord: func(code string) *model.ModelRecord {
			return records[code]
		},
		Now:              now,
		RetryDeadAfterMs: 24 * 3600 * 1000,
	})
	if len(got) != 2 || got[0].Code != "a" || got[1].Code != "c" {
		t.Fatalf("got %+v", got)
	}
}

func TestIsDeadRetryDue(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).UnixMilli()
	if !IsDeadRetryDue(rec("a", model.HealthDead, strp("2026-01-01T00:00:00.000Z"), nil), now, 24*3600*1000) {
		t.Fatal("should be due")
	}
	if IsDeadRetryDue(rec("a", model.HealthDead, strp("2026-01-02T00:00:01.000Z"), nil), now, 24*3600*1000) {
		t.Fatal("should wait")
	}
	if IsDeadRetryDue(rec("a", model.HealthDead, strp("2026-01-01T00:00:00.000Z"), nil), now, 0) {
		t.Fatal("retry disabled")
	}
	if IsDeadRetryDue(rec("a", model.HealthDead, nil, nil), now, 1000) {
		t.Fatal("missing timestamp must wait")
	}
}

func TestIsThrottled(t *testing.T) {
	now := time.Now().UnixMilli()
	if !IsThrottled(rec("a", model.HealthAlive, nil, strp(time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano))), now) {
		t.Fatal("should be throttled")
	}
	if IsThrottled(rec("a", model.HealthAlive, nil, strp(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))), now) {
		t.Fatal("cooldown finished")
	}
	if IsThrottled(nil, now) {
		t.Fatal("nil record")
	}
}

func failedOutcome(kind model.FailureKind, status int, fatal bool) AttemptOutcome {
	err := model.NormalizedError{
		Kind: kind, Label: string(kind), Status: intp(status), Code: strp(string(kind)),
		Message: string(kind), Fatal: fatal, Retryable: true,
	}
	return AttemptOutcome{OK: false, Error: err, LatencyMs: 1}
}

func TestFailoverSwitchesOnExhausted(t *testing.T) {
	calls := []string{}
	var dead []string
	result := RunFailover(FailoverInput{
		Candidates:  []string{"a", "b", "c"},
		MaxAttempts: 5,
		Attempt: func(ctx context.Context, code string) AttemptOutcome {
			calls = append(calls, code)
			if code == "a" {
				return failedOutcome(model.FailureQuotaExhausted, 403, false)
			}
			return AttemptOutcome{OK: true, Response: &http.Response{StatusCode: 200}, LatencyMs: 1}
		},
		Hooks: FailoverHooks{OnDead: func(code string, err model.NormalizedError) { dead = append(dead, code) }},
	})
	if !result.OK || result.Model != "b" || result.Failovers != 1 {
		t.Fatalf("got %+v", result)
	}
	if strings.Join(calls, ",") != "a,b" || strings.Join(dead, ",") != "a" {
		t.Fatalf("calls %v dead %v", calls, dead)
	}
}

func TestFailoverThrottleAndFatal(t *testing.T) {
	calls := []string{}
	result := RunFailover(FailoverInput{
		Candidates:  []string{"a", "b"},
		MaxAttempts: 4,
		Attempt: func(ctx context.Context, code string) AttemptOutcome {
			calls = append(calls, code)
			if code == "a" {
				return failedOutcome(model.FailureThrottled, 429, false)
			}
			return AttemptOutcome{OK: true, LatencyMs: 1}
		},
	})
	if !result.OK || strings.Join(calls, ",") != "a,b" {
		t.Fatalf("got %+v %v", result, calls)
	}

	calls = nil
	fatal := RunFailover(FailoverInput{
		Candidates:  []string{"a", "b", "c"},
		MaxAttempts: 5,
		Attempt: func(ctx context.Context, code string) AttemptOutcome {
			calls = append(calls, code)
			return failedOutcome(model.FailureAuth, 401, true)
		},
	})
	if fatal.OK || fatal.Error.Kind != model.FailureAuth || len(calls) != 1 {
		t.Fatalf("got %+v calls=%v", fatal, calls)
	}
}

func TestFailoverExhaustedReportsAttempts(t *testing.T) {
	result := RunFailover(FailoverInput{
		Candidates:  []string{"a", "b"},
		MaxAttempts: 5,
		Attempt: func(ctx context.Context, code string) AttemptOutcome {
			return failedOutcome(model.FailureQuotaExhausted, 403, false)
		},
	})
	if result.OK || !result.Exhausted || result.Error.Kind != model.FailureExhausted {
		t.Fatalf("got %+v", result)
	}
	if len(result.Attempts) != 2 || !strings.Contains(result.Error.Message, "a: quota_exhausted 403") {
		t.Fatalf("got %+v", result)
	}
}

func TestFailoverCapsAttemptsAndAborts(t *testing.T) {
	calls := []string{}
	RunFailover(FailoverInput{
		Candidates:  []string{"a", "b", "c"},
		MaxAttempts: 2,
		Attempt: func(ctx context.Context, code string) AttemptOutcome {
			calls = append(calls, code)
			return failedOutcome(model.FailureProvider, 500, false)
		},
	})
	if strings.Join(calls, ",") != "a,b" {
		t.Fatalf("calls %v", calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = nil
	aborted := RunFailover(FailoverInput{
		Context:     ctx,
		Candidates:  []string{"a", "b"},
		MaxAttempts: 3,
		Attempt: func(ctx context.Context, code string) AttemptOutcome {
			calls = append(calls, code)
			return AttemptOutcome{OK: true}
		},
	})
	if aborted.OK || aborted.Error.Kind != model.FailureAborted || len(calls) != 0 {
		t.Fatalf("got %+v calls=%v", aborted, calls)
	}
}

func TestFailoverAllThrottledSurface(t *testing.T) {
	result := RunFailover(FailoverInput{
		Candidates:  []string{"a", "b"},
		MaxAttempts: 5,
		Attempt: func(ctx context.Context, code string) AttemptOutcome {
			return failedOutcome(model.FailureThrottled, 429, false)
		},
	})
	if result.OK || result.Error.Kind != model.FailureThrottled || result.Error.Label != "所有模型均被限流" {
		t.Fatalf("got %+v", result)
	}
}

func TestFailoverProviderIDAndFirstTry(t *testing.T) {
	result := RunFailover(FailoverInput{
		Candidates:  []string{"a", "b"},
		MaxAttempts: 2,
		Attempt: func(ctx context.Context, code string) AttemptOutcome {
			return AttemptOutcome{OK: true, LatencyMs: 1, ProviderID: "alt"}
		},
	})
	if !result.OK || result.Failovers != 0 || result.Provider != "alt" || len(result.Attempts) != 1 {
		t.Fatalf("got %+v", result)
	}
}

func TestAPIKindForPath(t *testing.T) {
	cases := map[string]model.ApiKind{
		"chat/completions": model.ApiChat,
		"completions":      model.ApiCompletions,
		"responses":        model.ApiResponses,
		"embeddings":       model.ApiEmbeddings,
		"other":            model.ApiChat,
	}
	for path, want := range cases {
		if got := APIKindForPath(path); got != want {
			t.Fatalf("%s -> %s", path, got)
		}
	}
	if !SupportsAPI(map[string][]model.ApiKind{}, "x", model.ApiChat) {
		t.Fatal("default chat support")
	}
	if SupportsAPI(map[string][]model.ApiKind{}, "x", model.ApiResponses) {
		t.Fatal("default responses unsupported")
	}
}

func TestParseLeaderboardHTML(t *testing.T) {
	escaped := `{\"initialModels\":[{\"slug\":\"qwen3-8-max\",\"name\":\"Qwen3.8 Max\",\"intelligenceIndex\":50,\"contextWindowTokens\":262144,\"isReasoning\":false},{\"slug\":\"deepseek-v4-pro-2026-01-01\",\"name\":\"DeepSeek V4 Pro\",\"intelligenceIndex\":70}]}`
	html := `<script>self.__next_f.push([1,"` + escaped + `"])</script>`
	models := ParseLeaderboardHTML(html)
	if len(models) != 2 || models[0].Slug != "qwen3-8-max" || models[0].Score == nil || *models[0].Score != 50 {
		t.Fatalf("got %+v", models)
	}
	if models[1].ContextWindow != nil || models[1].Creator != nil {
		t.Fatalf("missing fields should stay nil: %+v", models[1])
	}
	if got := ParseLeaderboardHTML("<html></html>"); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestCapabilityIndexAndMatch(t *testing.T) {
	models := []CapabilityModel{
		{Slug: "qwen3-8-max", Name: "Qwen3.8 Max", Score: floatp(50)},
		{Slug: "qwen3-8-max-1019", Name: "Qwen3.8 Max 1019", Score: floatp(55)},
		{Slug: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", Score: floatp(70)},
		{Slug: "muse-glimmer", Name: "Muse Glimmer", Score: floatp(40)},
		{Slug: "unscored-model", Name: "Unscored"},
	}
	index := BuildCapabilityIndex(models)
	match, ok := MatchCapability(index, "qwen3.8-max")
	if !ok || match.Model.Slug != "qwen3-8-max" {
		t.Fatalf("got %+v ok=%v", match, ok)
	}
	if match, ok := MatchCapability(index, "muse-glimmer-30b"); !ok || match.Model.Slug != "muse-glimmer" {
		t.Fatalf("parameter alias: %+v ok=%v", match, ok)
	}
	if _, ok := MatchCapability(index, "unscored-model"); ok {
		t.Fatal("unscored models must not match")
	}
	if match, ok := MatchCapability(index, "deepseek-v4-pro:free"); !ok || match.Model.Slug != "deepseek-v4-pro" {
		t.Fatalf("tag suffix: %+v ok=%v", match, ok)
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"provider/Qwen3.8_Max":    "qwen3-8-max",
		"nvidia/deepseek/v4:free": "deepseek/v4",
		"  Model.Name  ":          "model-name",
		"a--b__c":                 "a-b-c",
	}
	for input, want := range cases {
		if got := NormalizeModelName(input); got != want {
			t.Fatalf("%q -> %q want %q", input, got, want)
		}
	}
}
