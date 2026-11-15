package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Schallophy/literouter/internal/config"
	"github.com/Schallophy/literouter/internal/model"
	"github.com/Schallophy/literouter/internal/runtime"
	"github.com/Schallophy/literouter/internal/util"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func fakeResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"content-type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func requestModel(t *testing.T, request *http.Request) string {
	t.Helper()
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	value, _ := body["model"].(string)
	return value
}

func newRuntime(t *testing.T, models []string) (*runtime.Runtime, string, model.GatewayConfig) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Providers = []model.ProviderConfig{{
		ID: "dashscope", Label: "Mock", BaseURL: "https://example.invalid/v1", APIKey: "sk-test",
		RequestTimeoutMs: 90_000, InjectStreamUsage: true,
	}}
	cfg.DefaultProvider = "dashscope"
	cfg.Chain.Models = append([]string(nil), models...)
	cfg.ModelApis = map[string][]model.ApiKind{}
	for _, code := range models {
		cfg.ModelApis[code] = []model.ApiKind{model.ApiChat}
	}
	path := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	rt, err := runtime.Create(runtime.Options{
		ConfigPath: path, DataDir: dir, Config: cfg, Logger: util.NopLogger(),
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	t.Cleanup(rt.Shutdown)
	return rt, path, cfg
}

func readSavedConfig(t *testing.T, path string) model.GatewayConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg model.GatewayConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg
}

func TestUpdateChainModelsDedupesAndPersists(t *testing.T) {
	rt, path, _ := newRuntime(t, nil)
	if err := rt.UpdateChainModels([]string{"dashscope/a", "dashscope/b", "dashscope/a"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	codes := rt.ChainCodes()
	if fmt.Sprint(codes) != "[dashscope/a dashscope/b]" {
		t.Fatalf("unexpected chain: %v", codes)
	}
	if primary := rt.PrimaryModel(); primary == nil || *primary != "dashscope/a" {
		t.Fatalf("unexpected primary: %v", primary)
	}
	saved := readSavedConfig(t, path)
	if fmt.Sprint(saved.Chain.Models) != "[dashscope/a dashscope/b]" {
		t.Fatalf("saved chain: %v", saved.Chain.Models)
	}
}

func TestAutoAlwaysStartsFromFirstModel(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a", "dashscope/b"})
	var calls []string
	rt.Client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, requestModel(t, request))
		return fakeResponse(200, `{"choices":[{"message":{"content":"ok"}}]}`), nil
	}))
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	first := rt.RequestThrough(runtime.RequestInput{Path: "chat/completions", Model: "auto", Body: body, Context: context.Background()})
	closeBody(t, first.Result.Response)
	if len(calls) != 1 || calls[0] != "a" {
		t.Fatalf("calls: %v", calls)
	}
	if err := rt.UpdateChainModels([]string{"dashscope/b", "dashscope/a"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	calls = nil
	second := rt.RequestThrough(runtime.RequestInput{Path: "chat/completions", Model: "auto", Body: body, Context: context.Background()})
	closeBody(t, second.Result.Response)
	if len(calls) != 1 || calls[0] != "b" {
		t.Fatalf("calls after reorder: %v", calls)
	}
}

func closeBody(t *testing.T, response *http.Response) {
	t.Helper()
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}

func TestRemovingFirstModelPromotesNext(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a", "dashscope/b", "dashscope/c"})
	if err := rt.UpdateChainModels([]string{"dashscope/b", "dashscope/c"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if primary := rt.PrimaryModel(); primary == nil || *primary != "dashscope/b" {
		t.Fatalf("primary: %v", primary)
	}
}

func TestEffectiveModelSkipsUnavailable(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a", "dashscope/b", "dashscope/c"})
	if value := rt.EffectiveModel(); value == nil || *value != "dashscope/a" {
		t.Fatalf("initial effective: %v", value)
	}
	rt.Store.SetHealth("dashscope/a", model.HealthDead, map[string]any{"reason": "额度已耗尽"})
	if value := rt.EffectiveModel(); value == nil || *value != "dashscope/b" {
		t.Fatalf("after dead: %v", value)
	}
	rt.Store.SetHealth("dashscope/b", model.HealthInvalid, map[string]any{"reason": "模型不存在"})
	if value := rt.EffectiveModel(); value == nil || *value != "dashscope/c" {
		t.Fatalf("after invalid: %v", value)
	}
	rt.Store.SetHealth("dashscope/c", model.HealthDead, map[string]any{"reason": "额度已耗尽"})
	if value := rt.EffectiveModel(); value != nil {
		t.Fatalf("expected nil, got %v", *value)
	}
}

func TestEffectiveModelSkipsCooldown(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a", "dashscope/b"})
	rt.Store.SetHealth("dashscope/a", model.HealthAlive, map[string]any{
		"reason": "限流", "throttledUntil": time.Now().Add(time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	if value := rt.EffectiveModel(); value == nil || *value != "dashscope/b" {
		t.Fatalf("cooling model should yield: %v", value)
	}
	rt.Store.SetHealth("dashscope/a", model.HealthAlive, map[string]any{
		"reason": nil, "throttledUntil": time.Now().Add(-time.Second).UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	if value := rt.EffectiveModel(); value == nil || *value != "dashscope/a" {
		t.Fatalf("cooled model should return: %v", value)
	}
}

func TestLastOutputFollowsServingModel(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a", "dashscope/b"})
	if rt.LastOutput() != nil {
		t.Fatal("expected no output yet")
	}
	failA := true
	rt.Client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		upstream := requestModel(t, request)
		if upstream == "a" && failA {
			failA = false
			return fakeResponse(403, `{"error":{"code":"AllocationQuota.FreeTierOnly","message":"quota"}}`), nil
		}
		return fakeResponse(200, `{"choices":[{"message":{"content":"ok"}}]}`), nil
	}))
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	auto := rt.RequestThrough(runtime.RequestInput{Path: "chat/completions", Model: "auto", Body: body, Context: context.Background()})
	closeBody(t, auto.Result.Response)
	output := rt.LastOutput()
	if output == nil || output.Model != "dashscope/b" || output.Mode != "auto" || output.Requested != "auto" {
		t.Fatalf("auto output: %+v", output)
	}
	direct := rt.RequestThrough(runtime.RequestInput{Path: "chat/completions", Model: "dashscope/a", Body: body, Context: context.Background()})
	closeBody(t, direct.Result.Response)
	output = rt.LastOutput()
	if output == nil || output.Model != "dashscope/a" || output.Mode != "direct" {
		t.Fatalf("direct output: %+v", output)
	}
}

func TestRetriedDeadModelThatFailsAgainGoesBackToWaiting(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a", "dashscope/b"})
	old := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	rt.Store.SetHealth("dashscope/a", model.HealthDead, map[string]any{"reason": "额度已耗尽", "checkedAt": old})
	if value := rt.EffectiveModel(); value == nil || *value != "dashscope/a" {
		t.Fatalf("expired dead model should be retried: %v", value)
	}
	status := 403
	code := "Forbidden"
	rt.MarkSkipped("dashscope/a", model.NormalizedError{
		Kind: model.FailureForbidden, Label: "无权限", Status: &status, Code: &code,
	})
	if value := rt.EffectiveModel(); value == nil || *value != "dashscope/b" {
		t.Fatalf("failed retry should yield: %v", value)
	}
}

func TestContextGuardUsesCalibration(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a", "dashscope/b"})
	window := 1000
	if _, err := rt.UpdateChainContext(&window, nil); err != nil {
		t.Fatalf("update context: %v", err)
	}
	rt.Store.ObserveCalibration("dashscope/a", false, 1000, 5000)
	rt.Client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return fakeResponse(200, `{"choices":[{"message":{"content":"ok"}}]}`), nil
	}))
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("word ", 400)}}}
	capped := rt.RequestThrough(runtime.RequestInput{Path: "chat/completions", Model: "dashscope/a", Body: body, Context: context.Background()})
	if capped.Result.OK {
		t.Fatal("expected context rejection")
	}
	if capped.Result.Error.Code == nil || *capped.Result.Error.Code != "literouter.context_length_exceeded" {
		t.Fatalf("code: %v", capped.Result.Error.Code)
	}
	if capped.Sample == nil || capped.Sample.HasTools {
		t.Fatalf("sample: %+v", capped.Sample)
	}
	uncapped := rt.RequestThrough(runtime.RequestInput{Path: "chat/completions", Model: "dashscope/b", Body: body, Context: context.Background()})
	if !uncapped.Result.OK {
		t.Fatalf("uncapped model should pass: %+v", uncapped.Result.Error)
	}
	closeBody(t, uncapped.Result.Response)
}

func TestMarkSuccessAccumulatesConfiguredCost(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a"})
	next := rt.ConfigSnapshot()
	next.Pricing = model.PricingConfig{
		Currency: "CNY", Unit: 1_000_000,
		Models: map[string]model.ModelPricing{
			"dashscope/a": {Timezone: "UTC", Tiers: []model.PriceTier{{
				From: "00:00", To: "00:00", Input: 2, CachedInput: 0.2, Output: 8,
			}}},
		},
	}
	if _, err := rt.ApplyConfig(next, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	usage := model.Usage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100, CachedPromptTokens: 800, CacheReported: true}
	rt.MarkSuccess("dashscope/a", &usage, model.AnonymousCaller, nil, model.ApiChat)
	cost := rt.Store.Stats().Cost
	if cost != 1360.0/1_000_000 {
		t.Fatalf("cost: %v", cost)
	}
	if record := rt.Store.Get("dashscope/a"); record == nil || record.Cost != cost {
		t.Fatalf("model cost: %+v", record)
	}
	callers := rt.Store.Callers()
	if len(callers) == 0 || callers[0].Cost != cost {
		t.Fatalf("caller cost: %+v", callers)
	}
}

func TestContextUpdatesValidateAndPersist(t *testing.T) {
	rt, path, _ := newRuntime(t, nil)
	window := 64_000
	overflow := model.OverflowPassthrough
	next, err := rt.UpdateChainContext(&window, &overflow)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if next.Window != 64_000 || next.OnOverflow != model.OverflowPassthrough {
		t.Fatalf("context: %+v", next)
	}
	if saved := readSavedConfig(t, path); saved.Chain.Context.Window != 64_000 {
		t.Fatalf("saved context: %+v", saved.Chain.Context)
	}
	invalid := 999
	if _, err := rt.UpdateChainContext(&invalid, nil); err == nil || !strings.Contains(err.Error(), "context.window") {
		t.Fatalf("expected window error, got %v", err)
	}
}

func TestApplyConfigCanSkipPersistence(t *testing.T) {
	rt, path, _ := newRuntime(t, []string{"dashscope/a", "dashscope/b"})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	next := rt.ConfigSnapshot()
	next.Chain.Context.Window = 128_000
	warnings, err := rt.ApplyConfig(next, false)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("apply: %v %v", warnings, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("config file must not be rewritten")
	}
	if rt.ConfigSnapshot().Chain.Context.Window != 128_000 {
		t.Fatal("runtime config not updated")
	}
}

func TestApplyConfigWarnsOnHostAndPortChanges(t *testing.T) {
	rt, _, _ := newRuntime(t, nil)
	next := rt.ConfigSnapshot()
	next.Port = 9999
	next.Host = "0.0.0.0"
	warnings, err := rt.ApplyConfig(next, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings: %v", warnings)
	}
}

func TestResponsesChainingSendsOnlyTheDelta(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a"})
	next := rt.ConfigSnapshot()
	next.ResponsesChaining.Enabled = true
	next.ModelApis["dashscope/a"] = []model.ApiKind{model.ApiChat, model.ApiResponses}
	if _, err := rt.ApplyConfig(next, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var bodies []map[string]any
	rt.Client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(request.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		return fakeResponse(200, `{"id":"resp_1","object":"response","status":"completed","output":[]}`), nil
	}))
	first := rt.RequestThrough(runtime.RequestInput{
		Path: "responses", Model: "dashscope/a", Context: context.Background(),
		Body: map[string]any{"input": []any{map[string]any{"role": "user", "content": "hello"}}},
	})
	if !first.Result.OK {
		t.Fatalf("first failed: %+v", first.Result.Error)
	}
	closeBody(t, first.Result.Response)
	second := rt.RequestThrough(runtime.RequestInput{
		Path: "responses", Model: "dashscope/a", Context: context.Background(),
		Body: map[string]any{"input": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "user", "content": "again"},
		}},
	})
	if !second.Result.OK {
		t.Fatalf("second failed: %+v", second.Result.Error)
	}
	closeBody(t, second.Result.Response)
	if len(bodies) != 2 {
		t.Fatalf("bodies: %d", len(bodies))
	}
	if bodies[0]["previous_response_id"] != nil {
		t.Fatalf("first should not chain: %+v", bodies[0])
	}
	if bodies[0]["store"] != true {
		t.Fatalf("first should ask provider to store: %+v", bodies[0])
	}
	if bodies[1]["previous_response_id"] != "resp_1" {
		t.Fatalf("second previous id: %+v", bodies[1])
	}
	input, _ := bodies[1]["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("second input should be a delta: %+v", bodies[1]["input"])
	}
	stats := rt.ChainStats()
	if stats.Requests != 2 || stats.Hits != 1 || stats.Sessions != 2 {
		t.Fatalf("chain stats: %+v", stats)
	}
}

func TestSnapshotModelCarriesRoutingMetadata(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/qwen3-27b"})
	snapshot := rt.SnapshotModel("dashscope/qwen3-27b")
	if snapshot.Upstream != "qwen3-27b" || snapshot.Provider != "dashscope" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	if len(snapshot.APIs) != 1 || snapshot.APIs[0] != model.ApiChat {
		t.Fatalf("apis: %v", snapshot.APIs)
	}
	if snapshot.Family != "Qwen" {
		t.Fatalf("family: %s", snapshot.Family)
	}
	chain := rt.ChainSnapshot()
	if len(chain.Models) != 1 || chain.Effective == nil {
		t.Fatalf("chain: %+v", chain)
	}
	unknown := rt.SnapshotModel("dashscope/missing")
	if unknown.Health != model.HealthUnknown {
		t.Fatalf("unknown health: %s", unknown.Health)
	}
}

func TestReviveAndResetUsageAndBootstrap(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a"})
	rt.Store.SetHealth("dashscope/a", model.HealthDead, map[string]any{"reason": "额度已耗尽"})
	rt.Revive("dashscope/a")
	if record := rt.Store.Get("dashscope/a"); record == nil || record.Health != model.HealthUnknown {
		t.Fatalf("revive: %+v", record)
	}
	rt.Store.BumpRequests(model.ApiChat)
	rt.ResetUsage()
	if stats := rt.Store.Stats(); stats.Requests != 0 || stats.Failovers != 0 {
		t.Fatalf("reset: %+v", stats)
	}
	rt.Bootstrap()
	events := rt.Events.Recent(10, "")
	found := false
	for _, event := range events {
		if strings.Contains(event.Message, "literouter 已启动") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing start event: %+v", events)
	}
}

func TestSanitizedConfigHidesKeys(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a"})
	sanitized := rt.SanitizedConfig()
	if sanitized.Providers[0].APIKey != "" {
		t.Fatal("api key leaked")
	}
	if !strings.Contains(rt.SanitizedConfigText(), `"apiKey": ""`) {
		t.Fatal("sanitized text should blank the key")
	}
	next := rt.ConfigSnapshot()
	next.Providers[0].APIKey = ""
	if _, err := rt.SetConfigFromAdmin(next); err != nil {
		t.Fatalf("admin apply: %v", err)
	}
	if rt.ConfigSnapshot().Providers[0].APIKey != "sk-test" {
		t.Fatal("inline key should be preserved when the form leaves it blank")
	}
}

func TestDirectModelNotInChainIsPassthrough(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/a"})
	var calls []string
	rt.Client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, requestModel(t, request))
		return fakeResponse(200, `{"choices":[]}`), nil
	}))
	result := rt.RequestThrough(runtime.RequestInput{
		Path: "chat/completions", Model: "dashscope/unknown", Context: context.Background(),
		Body: map[string]any{"messages": []any{}},
	})
	if !result.Result.OK {
		t.Fatalf("passthrough failed: %+v", result.Result.Error)
	}
	closeBody(t, result.Result.Response)
	if len(calls) != 1 || calls[0] != "unknown" {
		t.Fatalf("calls: %v", calls)
	}
	if output := rt.LastOutput(); output == nil || output.Mode != "passthrough" {
		t.Fatalf("mode: %+v", output)
	}
}

func TestBareUniqueModelNameResolvesDirectly(t *testing.T) {
	rt, _, _ := newRuntime(t, []string{"dashscope/qwen3-27b", "dashscope/other"})
	var calls []string
	rt.Client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls = append(calls, requestModel(t, request))
		return fakeResponse(200, `{"choices":[]}`), nil
	}))
	result := rt.RequestThrough(runtime.RequestInput{
		Path: "chat/completions", Model: "qwen3-27b", Context: context.Background(),
		Body: map[string]any{"messages": []any{}},
	})
	if !result.Result.OK {
		t.Fatalf("request failed: %+v", result.Result.Error)
	}
	closeBody(t, result.Result.Response)
	if len(calls) != 1 || calls[0] != "qwen3-27b" {
		t.Fatalf("calls: %v", calls)
	}
	if output := rt.LastOutput(); output == nil || output.Mode != "direct" || output.Model != "dashscope/qwen3-27b" {
		t.Fatalf("output: %+v", output)
	}
}
