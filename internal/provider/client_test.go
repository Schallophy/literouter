package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Schallophy/literouter/internal/model"
)

func twoProviderConfig() model.GatewayConfig {
	config := model.GatewayConfig{
		Providers: []model.ProviderConfig{
			{ID: "alpha", Label: "Alpha", BaseURL: "https://alpha.example/v1", APIKeyEnv: "LITEROUTER_TEST_ALPHA_KEY", RequestTimeoutMs: 1000, InjectStreamUsage: true},
			{ID: "beta", Label: "Beta", BaseURL: "https://beta.example/v1", APIKeyEnv: "LITEROUTER_TEST_BETA_KEY", APIKey: "sk-beta-inline", RequestTimeoutMs: 1000},
		},
		DefaultProvider: "beta",
	}
	return config
}

type callRecord struct {
	path   string
	header http.Header
	body   map[string]any
}

func stubServer(t *testing.T, status int, body string) (*httptest.Server, *[]callRecord) {
	t.Helper()
	var calls []callRecord
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		calls = append(calls, callRecord{path: request.URL.Path, header: request.Header.Clone(), body: decoded})
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func TestResolveProviderAndKeys(t *testing.T) {
	config := twoProviderConfig()
	if provider := ResolveProvider(&config, ""); provider == nil || provider.ID != "beta" {
		t.Fatalf("default provider: %+v", provider)
	}
	if provider := ResolveProvider(&config, "ALPHA"); provider == nil || provider.ID != "alpha" {
		t.Fatalf("explicit provider: %+v", provider)
	}
	if provider := ResolveProvider(&config, "ghost"); provider != nil {
		t.Fatalf("ghost should be nil: %+v", provider)
	}
	t.Setenv("LITEROUTER_TEST_ALPHA_KEY", "")
	alpha := ResolveProvider(&config, "alpha")
	info := KeyInfoFor(alpha)
	if info.Configured || info.Source != "none" {
		t.Fatalf("alpha key info: %+v", info)
	}
	beta := ResolveProvider(&config, "beta")
	info = KeyInfoFor(beta)
	if !info.Configured || info.Source != "config" {
		t.Fatalf("beta key info: %+v", info)
	}
}

func TestAttemptUsesDefaultProviderAndBody(t *testing.T) {
	server, calls := stubServer(t, 200, `{"ok":true}`)
	config := twoProviderConfig()
	config.Providers[1].BaseURL = server.URL + "/v1"
	client := NewClient(func() model.GatewayConfig { return config })

	outcome := client.Attempt(context.Background(), "chat/completions", "upstream-model", map[string]any{"messages": []any{}}, AttemptOptions{})
	if !outcome.OK || outcome.ProviderID != "beta" || outcome.Response == nil {
		t.Fatalf("outcome %+v", outcome)
	}
	outcome.Response.Body.Close()
	if len(*calls) != 1 {
		t.Fatalf("calls %d", len(*calls))
	}
	call := (*calls)[0]
	if call.path != "/v1/chat/completions" {
		t.Fatalf("path %s", call.path)
	}
	if call.header.Get("authorization") != "Bearer sk-beta-inline" {
		t.Fatalf("auth %q", call.header.Get("authorization"))
	}
	if call.body["model"] != "upstream-model" {
		t.Fatalf("body %+v", call.body)
	}
	if !strings.HasPrefix(call.header.Get("user-agent"), "literouter/") {
		t.Fatalf("user agent %q", call.header.Get("user-agent"))
	}
}

func TestAttemptUnknownProvider(t *testing.T) {
	server, calls := stubServer(t, 200, `{}`)
	config := twoProviderConfig()
	config.Providers[1].BaseURL = server.URL
	client := NewClient(func() model.GatewayConfig { return config })
	outcome := client.Attempt(context.Background(), "chat/completions", "m", map[string]any{}, AttemptOptions{ProviderID: "ghost"})
	if outcome.OK || outcome.Error.Code == nil || *outcome.Error.Code != "literouter.provider_unknown" || !outcome.Error.Fatal {
		t.Fatalf("outcome %+v", outcome)
	}
	if len(*calls) != 0 {
		t.Fatal("must not call the network")
	}
}

func TestAttemptMissingKey(t *testing.T) {
	server, calls := stubServer(t, 200, `{}`)
	config := twoProviderConfig()
	config.Providers[0].BaseURL = server.URL
	t.Setenv("LITEROUTER_TEST_ALPHA_KEY", "")
	client := NewClient(func() model.GatewayConfig { return config })
	outcome := client.Attempt(context.Background(), "chat/completions", "m", map[string]any{}, AttemptOptions{ProviderID: "alpha"})
	if outcome.OK || outcome.Error.Code == nil || *outcome.Error.Code != "literouter.no_key" {
		t.Fatalf("outcome %+v", outcome)
	}
	if !strings.Contains(outcome.Error.Message, "alpha") {
		t.Fatalf("message %q", outcome.Error.Message)
	}
	if len(*calls) != 0 {
		t.Fatal("must not call the network")
	}
}

func TestAttemptClassifiesHTTPErrors(t *testing.T) {
	server, _ := stubServer(t, 403, `{"error":{"code":"FreeTierOnly","message":"free tier"}}`)
	config := twoProviderConfig()
	config.Providers[1].BaseURL = server.URL
	client := NewClient(func() model.GatewayConfig { return config })
	outcome := client.Attempt(context.Background(), "chat/completions", "m", map[string]any{}, AttemptOptions{})
	if outcome.OK || outcome.Error.Kind != model.FailureQuotaExhausted || !outcome.Error.MarkDead {
		t.Fatalf("outcome %+v", outcome)
	}
}

func TestAttemptTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writer.WriteHeader(200)
	}))
	defer server.Close()
	config := twoProviderConfig()
	config.Providers[1].BaseURL = server.URL
	client := NewClient(func() model.GatewayConfig { return config })

	outcome := client.Attempt(context.Background(), "chat/completions", "m", map[string]any{}, AttemptOptions{TimeoutMs: 20})
	if outcome.OK || outcome.Error.Kind != model.FailureTimeout {
		t.Fatalf("timeout outcome %+v", outcome)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome = client.Attempt(ctx, "chat/completions", "m", map[string]any{}, AttemptOptions{})
	if outcome.OK || outcome.Error.Kind != model.FailureAborted || !outcome.Error.Fatal {
		t.Fatalf("aborted outcome %+v", outcome)
	}
}

func TestListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Errorf("path %s", request.URL.Path)
		}
		writer.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":"a"},{"id":" "}]}`))
	}))
	defer server.Close()
	config := twoProviderConfig()
	config.Providers[1].BaseURL = server.URL + "/v1"
	client := NewClient(func() model.GatewayConfig { return config })
	ids, err := client.ListModels(context.Background(), "beta")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("ids %v", ids)
	}
}

func TestMaskKey(t *testing.T) {
	if got := MaskKey("short"); got != "••••••" {
		t.Fatalf("got %q", got)
	}
	if got := MaskKey("sk-1234567890"); got != "sk-…7890" {
		t.Fatalf("got %q", got)
	}
}
