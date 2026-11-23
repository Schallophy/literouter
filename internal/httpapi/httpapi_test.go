package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Schallophy/literouter/internal/config"
	"github.com/Schallophy/literouter/internal/httpapi"
	"github.com/Schallophy/literouter/internal/model"
	"github.com/Schallophy/literouter/internal/runtime"
	"github.com/Schallophy/literouter/internal/stream"
	"github.com/Schallophy/literouter/internal/util"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func fakeResponse(status int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

type harness struct {
	t      *testing.T
	rt     *runtime.Runtime
	cfg    model.GatewayConfig
	base   string
	path   string
	api    *httpapi.Server
	server *httptest.Server
}

func newHarness(t *testing.T, mutate func(*model.GatewayConfig)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Providers = []model.ProviderConfig{{
		ID: "dashscope", Label: "Mock", BaseURL: "https://example.invalid/v1", APIKey: "sk-test",
		RequestTimeoutMs: 90_000, InjectStreamUsage: true,
	}}
	cfg.DefaultProvider = "dashscope"
	cfg.Chain.Models = []string{"dashscope/a", "dashscope/b"}
	cfg.ModelApis = map[string][]model.ApiKind{
		"dashscope/a": {model.ApiChat, model.ApiResponses},
		"dashscope/b": {model.ApiChat, model.ApiResponses, model.ApiEmbeddings},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	path := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	rt, err := runtime.Create(runtime.Options{ConfigPath: path, DataDir: dir, Config: cfg, Logger: util.NopLogger()})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	t.Cleanup(rt.Shutdown)
	api := httpapi.NewServer(rt, util.NopLogger())
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	return &harness{t: t, rt: rt, cfg: cfg, base: server.URL, path: path, api: api, server: server}
}

func (h *harness) do(method, path string, body any, headers map[string]string) *http.Response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, h.base+path, reader)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("do: %v", err)
	}
	return response
}

func decode(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return data
}

func TestHealthReportsChainAndProviders(t *testing.T) {
	h := newHarness(t, nil)
	response := h.do("GET", "/health", nil, nil)
	if response.StatusCode != 200 || response.Header.Get("cache-control") != "no-store" {
		t.Fatalf("status/headers: %d %v", response.StatusCode, response.Header)
	}
	data := decode(t, response)
	if data["status"] != "ok" || data["model"] != "dashscope/a" {
		t.Fatalf("health: %+v", data)
	}
	models := data["models"].(map[string]any)
	if models["total"].(float64) != 2 {
		t.Fatalf("models: %+v", models)
	}
}

func TestGatewayTokenIsRequiredAndTimingSafe(t *testing.T) {
	h := newHarness(t, func(cfg *model.GatewayConfig) { cfg.GatewayToken = "secret-token" })
	unauthorized := h.do("GET", "/v1/models", nil, nil)
	data := decode(t, unauthorized)
	if unauthorized.StatusCode != 401 {
		t.Fatalf("status: %d", unauthorized.StatusCode)
	}
	errorBody := data["error"].(map[string]any)
	if errorBody["code"] != "literouter.unauthorized" {
		t.Fatalf("error: %+v", errorBody)
	}
	authorized := h.do("GET", "/v1/models", nil, map[string]string{"authorization": "Bearer secret-token"})
	if authorized.StatusCode != 200 {
		t.Fatalf("authorized status: %d", authorized.StatusCode)
	}
	authorized.Body.Close()
	wrong := h.do("GET", "/v1/models", nil, map[string]string{"authorization": "Bearer secret-tokens"})
	if wrong.StatusCode != 401 {
		t.Fatalf("wrong token status: %d", wrong.StatusCode)
	}
	wrong.Body.Close()
}

func TestCORSPreflight(t *testing.T) {
	h := newHarness(t, nil)
	response := h.do("OPTIONS", "/v1/chat/completions", nil, nil)
	defer response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("status: %d", response.StatusCode)
	}
	if response.Header.Get("access-control-allow-origin") != "*" {
		t.Fatalf("cors: %v", response.Header)
	}
	if !strings.Contains(response.Header.Get("access-control-expose-headers"), "x-literouter-model") {
		t.Fatalf("expose headers: %v", response.Header)
	}
}

func TestAdminRejectsNonLoopback(t *testing.T) {
	h := newHarness(t, nil)
	request := httptest.NewRequest("GET", "/api/overview", nil)
	request.RemoteAddr = "10.1.2.3:5555"
	recorder := httptest.NewRecorder()
	httpapi.NewServer(h.rt, util.NopLogger()).ServeHTTP(recorder, request)
	if recorder.Code != 403 {
		t.Fatalf("status: %d", recorder.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if body["error"].(map[string]any)["code"] != "literouter.remote_admin" {
		t.Fatalf("body: %v", recorder.Body.String())
	}
}

func TestStaticAssetsAreEmbeddedAndNoStore(t *testing.T) {
	h := newHarness(t, nil)
	response := h.do("GET", "/", nil, nil)
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("cache-control") != "no-store" {
		t.Fatalf("static: %d %v", response.StatusCode, response.Header)
	}
	if !bytes.Contains(raw, []byte("literouter")) {
		t.Fatal("index should carry the literouter brand")
	}

	request := httptest.NewRequest("GET", "/../config.json", nil)
	recorder := httptest.NewRecorder()
	h.server.Config.Handler.ServeHTTP(recorder, request)
	if recorder.Code != 404 {
		t.Fatalf("traversal status: %d", recorder.Code)
	}

	head := httptest.NewRequest("HEAD", "/", nil)
	headRecorder := httptest.NewRecorder()
	h.server.Config.Handler.ServeHTTP(headRecorder, head)
	if headRecorder.Code != 200 || headRecorder.Body.Len() != 0 {
		t.Fatalf("head: %d len=%d", headRecorder.Code, headRecorder.Body.Len())
	}
}

func TestBodyLimitAndInvalidJSON(t *testing.T) {
	h := newHarness(t, func(cfg *model.GatewayConfig) { cfg.Server.MaxBodyBytes = 64 })
	huge := map[string]any{"messages": []any{map[string]any{"content": strings.Repeat("x", 128)}}}
	response := h.do("POST", "/v1/chat/completions", huge, nil)
	data := decode(t, response)
	if response.StatusCode != 413 || !strings.Contains(data["error"].(map[string]any)["message"].(string), "请求体超过") {
		t.Fatalf("limit: %d %+v", response.StatusCode, data)
	}
	request, _ := http.NewRequest("POST", h.base+"/v1/chat/completions", strings.NewReader("{ nope"))
	request.Header.Set("content-type", "application/json")
	bad, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	badData := decode(t, bad)
	if bad.StatusCode != 400 || badData["error"].(map[string]any)["message"] != "请求体不是合法 JSON" {
		t.Fatalf("invalid json: %d %+v", bad.StatusCode, badData)
	}
}

func TestModelsHidesDeadModelsAndReportsAuto(t *testing.T) {
	h := newHarness(t, nil)
	h.rt.Store.SetHealth("dashscope/b", model.HealthDead, map[string]any{"reason": "额度已耗尽"})
	response := h.do("GET", "/v1/models", nil, nil)
	data := decode(t, response)
	list := data["data"].([]any)
	if len(list) != 2 {
		t.Fatalf("expected auto + one live model, got %d", len(list))
	}
	auto := list[0].(map[string]any)
	if auto["id"] != "auto" || auto["owned_by"] != "literouter" {
		t.Fatalf("auto: %+v", auto)
	}
	if auto["active"] != "dashscope/a" {
		t.Fatalf("active: %+v", auto["active"])
	}
}

func TestMissingKeyReturns503(t *testing.T) {
	h := newHarness(t, func(cfg *model.GatewayConfig) { cfg.Providers[0].APIKey = "" })
	response := h.do("POST", "/v1/chat/completions", map[string]any{
		"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	data := decode(t, response)
	if response.StatusCode != 503 || response.Header.Get("retry-after") != "30" {
		t.Fatalf("status: %d %v", response.StatusCode, response.Header)
	}
	errorBody := data["error"].(map[string]any)
	if errorBody["code"] != "literouter.no_key" || errorBody["type"] != "literouter.auth" {
		t.Fatalf("error: %+v", errorBody)
	}
}

func TestChatProxyFailoverAndStream(t *testing.T) {
	h := newHarness(t, nil)
	var lock sync.Mutex
	var seen []string
	h.rt.Client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(request.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		name, _ := body["model"].(string)
		lock.Lock()
		seen = append(seen, name)
		lock.Unlock()
		if name == "a" {
			return fakeResponse(403, "application/json", `{"error":{"code":"AllocationQuota.FreeTierOnly","message":"quota"}}`), nil
		}
		if body["stream"] == true {
			sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
			return fakeResponse(200, "text/event-stream", sse), nil
		}
		return fakeResponse(200, "application/json", `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`), nil
	}))

	response := h.do("POST", "/v1/chat/completions", map[string]any{
		"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("x-literouter-model") != "dashscope/b" || response.Header.Get("x-literouter-failovers") != "1" {
		t.Fatalf("headers: %d %v body=%s", response.StatusCode, response.Header, raw)
	}
	if !bytes.Contains(raw, []byte(`"content":"ok"`)) {
		t.Fatalf("body: %s", raw)
	}

	stream := h.do("POST", "/v1/chat/completions", map[string]any{
		"model": "auto", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	streamRaw, _ := io.ReadAll(stream.Body)
	stream.Body.Close()
	if stream.StatusCode != 200 || stream.Header.Get("content-type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("stream: %d %v", stream.StatusCode, stream.Header)
	}
	if !bytes.Contains(streamRaw, []byte("data: [DONE]")) || !bytes.Contains(streamRaw, []byte(`"content":"hi"`)) {
		t.Fatalf("stream body: %s", streamRaw)
	}
	live := h.rt.LiveOutput.Recent(10)
	if len(live) == 0 || live[len(live)-1].Status != "ok" {
		t.Fatalf("live sessions: %+v", live)
	}
}

func TestAdminConfigNeverLeaksKeys(t *testing.T) {
	h := newHarness(t, nil)
	response := h.do("GET", "/api/config", nil, nil)
	data := decode(t, response)
	if data["apiKeySet"] != true {
		t.Fatalf("key set: %+v", data)
	}
	text := data["text"].(string)
	if strings.Contains(text, "sk-test") {
		t.Fatal("config text leaked the key")
	}
	configBody := data["config"].(map[string]any)
	provider := configBody["providers"].([]any)[0].(map[string]any)
	if provider["apiKey"] != "" {
		t.Fatal("config object leaked the key")
	}

	invalid := h.do("PUT", "/api/config", map[string]any{"text": "{ nope"}, nil)
	invalidData := decode(t, invalid)
	if invalid.StatusCode != 400 || len(invalidData["errors"].([]any)) == 0 {
		t.Fatalf("invalid config: %d %+v", invalid.StatusCode, invalidData)
	}

	valid := h.do("PUT", "/api/config", map[string]any{"config": map[string]any{
		"chain": map[string]any{"context": map[string]any{"window": 64000}},
	}}, nil)
	validData := decode(t, valid)
	if valid.StatusCode != 200 || validData["ok"] != true {
		t.Fatalf("valid config: %d %+v", valid.StatusCode, validData)
	}
	if h.rt.ConfigSnapshot().Chain.Context.Window != 64000 {
		t.Fatal("config not applied")
	}
}

func TestChainPatchAndModelManagement(t *testing.T) {
	h := newHarness(t, nil)
	bad := h.do("PATCH", "/api/chain", map[string]any{"context": map[string]any{"window": 12}}, nil)
	if bad.StatusCode != 400 {
		t.Fatalf("bad window: %d", bad.StatusCode)
	}
	bad.Body.Close()
	good := h.do("PATCH", "/api/chain", map[string]any{"context": map[string]any{"window": 64000, "onOverflow": "passthrough"}}, nil)
	goodData := decode(t, good)
	if good.StatusCode != 200 || goodData["context"].(map[string]any)["onOverflow"] != "passthrough" {
		t.Fatalf("patch: %d %+v", good.StatusCode, goodData)
	}

	add := h.do("POST", "/api/chain/models", map[string]any{"code": "dashscope/c"}, nil)
	if add.StatusCode != 200 {
		t.Fatalf("add: %d", add.StatusCode)
	}
	add.Body.Close()
	if fmt.Sprint(h.rt.ChainCodes()) != "[dashscope/a dashscope/b dashscope/c]" {
		t.Fatalf("chain: %v", h.rt.ChainCodes())
	}
	remove := h.do("POST", "/api/chain/models/remove", map[string]any{"models": []any{"dashscope/c"}}, nil)
	removeBody := decode(t, remove)
	if remove.StatusCode != 200 || removeBody["removed"].(float64) != 1 {
		t.Fatalf("remove: %d %+v", remove.StatusCode, removeBody)
	}
	missing := h.do("DELETE", "/api/chain/models/dashscope%2Fmissing", nil, nil)
	if missing.StatusCode != 404 {
		t.Fatalf("delete missing: %d", missing.StatusCode)
	}
	missing.Body.Close()
}

func TestEventsAndLiveEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	h.rt.Events.Push(stream.EventInput{Kind: stream.EventStart, Level: stream.LevelInfo, Message: "已启动"})
	response := h.do("GET", "/api/events?limit=999&kind=failover", nil, nil)
	data := decode(t, response)
	if len(data["events"].([]any)) != 0 {
		t.Fatalf("events: %+v", data)
	}
	clear := h.do("DELETE", "/api/live", nil, nil)
	if clear.StatusCode != 200 {
		t.Fatalf("clear: %d", clear.StatusCode)
	}
	clear.Body.Close()
	raw := h.do("GET", "/api/live/999/raw", nil, nil)
	if raw.StatusCode != 404 {
		t.Fatalf("raw: %d", raw.StatusCode)
	}
	raw.Body.Close()
}

func TestOverviewStatsAndCallers(t *testing.T) {
	h := newHarness(t, nil)
	h.rt.Client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return fakeResponse(200, "application/json", `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`), nil
	}))
	response := h.do("POST", "/v1/chat/completions", map[string]any{
		"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, map[string]string{"authorization": "Bearer sk-caller-12345678"})
	response.Body.Close()
	time.Sleep(50 * time.Millisecond)
	overview := h.do("GET", "/api/overview", nil, nil)
	data := decode(t, overview)
	stats := data["stats"].(map[string]any)
	if stats["requests"].(float64) != 1 || stats["promptTokens"].(float64) != 10 {
		t.Fatalf("stats: %+v", stats)
	}
	apis := stats["apis"].(map[string]any)
	chat := apis["chat"].(map[string]any)
	if chat["requests"].(float64) != 1 || chat["completionTokens"].(float64) != 5 {
		t.Fatalf("api stats: %+v", chat)
	}
	callers := data["callers"].([]any)
	if len(callers) == 0 {
		t.Fatalf("callers: %+v", callers)
	}
	caller := callers[0].(map[string]any)
	if caller["label"] == "" || len(caller["models"].([]any)) != 1 {
		t.Fatalf("caller: %+v", caller)
	}
	output := data["output"].(map[string]any)
	if output["model"] != "dashscope/a" || output["mode"] != "auto" {
		t.Fatalf("output: %+v", output)
	}
}

func TestSelfTestEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	h.rt.Client.SetTransport(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return fakeResponse(200, "application/json", `{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`), nil
	}))
	response := h.do("POST", "/api/test", map[string]any{"prompt": "ping"}, nil)
	data := decode(t, response)
	if response.StatusCode != 200 || data["ok"] != true || data["content"] != "pong" {
		t.Fatalf("test: %d %+v", response.StatusCode, data)
	}
	if data["model"] != "dashscope/a" {
		t.Fatalf("model: %+v", data["model"])
	}
}

func TestStaticCanBeDisabled(t *testing.T) {
	h := newHarness(t, func(cfg *model.GatewayConfig) { cfg.UI.Enabled = false })
	response := h.do("GET", "/", nil, nil)
	data := decode(t, response)
	if response.StatusCode != 404 || data["error"].(map[string]any)["code"] != "literouter.ui_disabled" {
		t.Fatalf("ui disabled: %d %+v", response.StatusCode, data)
	}
}
