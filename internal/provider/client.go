// Package provider implements the upstream OpenAI-compatible HTTP client.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Schallophy/literouter/internal/core"
	"github.com/Schallophy/literouter/internal/model"
)

// Version is reported in the upstream user agent.
const Version = "0.1.0"

// KeyInfo describes a provider's resolved API key without exposing it.
type KeyInfo struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Configured bool   `json:"configured"`
	Source     string `json:"source"`
	Masked     string `json:"masked"`
}

// MaskKey keeps the first three and last four characters of a long key.
func MaskKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if len([]rune(trimmed)) <= 8 {
		return "••••••"
	}
	runes := []rune(trimmed)
	return string(runes[:3]) + "…" + string(runes[len(runes)-4:])
}

// ResolveProvider finds a provider by id, defaulting to the configured one.
// An explicitly requested but unknown id returns nil.
func ResolveProvider(config *model.GatewayConfig, id string) *model.ProviderConfig {
	requested := id
	if requested == "" {
		requested = config.DefaultProvider
	}
	wanted := strings.ToLower(strings.TrimSpace(requested))
	for i := range config.Providers {
		if config.Providers[i].ID == wanted {
			return &config.Providers[i]
		}
	}
	if id != "" {
		return nil
	}
	if len(config.Providers) > 0 {
		return &config.Providers[0]
	}
	return nil
}

// KeyInfoFor resolves an environment or inline key for one provider.
func KeyInfoFor(provider *model.ProviderConfig) KeyInfo {
	envName := strings.TrimSpace(provider.APIKeyEnv)
	if envName != "" {
		if value, ok := os.LookupEnv(envName); ok && strings.TrimSpace(value) != "" {
			return KeyInfo{ID: provider.ID, Label: provider.Label, Configured: true, Source: "env", Masked: MaskKey(value)}
		}
	}
	if inline := strings.TrimSpace(provider.APIKey); inline != "" {
		return KeyInfo{ID: provider.ID, Label: provider.Label, Configured: true, Source: "config", Masked: MaskKey(inline)}
	}
	return KeyInfo{ID: provider.ID, Label: provider.Label, Configured: false, Source: "none", Masked: ""}
}

// ResolveKeyInfo reports the default provider's key state.
func ResolveKeyInfo(config *model.GatewayConfig) KeyInfo {
	provider := ResolveProvider(config, "")
	if provider == nil {
		return KeyInfo{ID: config.DefaultProvider, Label: config.DefaultProvider, Configured: false, Source: "none", Masked: ""}
	}
	return KeyInfoFor(provider)
}

// ResolveAPIKey returns the usable key, if any.
func ResolveAPIKey(provider *model.ProviderConfig) (string, bool) {
	info := KeyInfoFor(provider)
	if !info.Configured {
		return "", false
	}
	if info.Source == "env" {
		value := strings.TrimSpace(os.Getenv(strings.TrimSpace(provider.APIKeyEnv)))
		return value, value != ""
	}
	value := strings.TrimSpace(provider.APIKey)
	return value, value != ""
}

// AttemptOptions overrides the provider and timeout for one attempt.
type AttemptOptions struct {
	ProviderID string
	TimeoutMs  int
}

// Client performs upstream requests against the current configuration.
type Client struct {
	getConfig func() model.GatewayConfig
	transport http.RoundTripper
}

// NewClient builds a client reading configuration lazily.
func NewClient(getConfig func() model.GatewayConfig) *Client {
	return &Client{getConfig: getConfig}
}

// SetTransport allows tests and deployments to share a tuned transport.
func (c *Client) SetTransport(transport http.RoundTripper) { c.transport = transport }

func (c *Client) httpClient() *http.Client {
	return &http.Client{Transport: c.transport} // no Timeout: SSE must stay open
}

// Attempt sends one provider request. The returned response body is owned by the
// caller and must be closed.
func (c *Client) Attempt(ctx context.Context, path, modelCode string, body map[string]any, options AttemptOptions) core.AttemptOutcome {
	config := c.getConfig()
	provider := ResolveProvider(&config, options.ProviderID)
	if provider == nil {
		requested := options.ProviderID
		if requested == "" {
			requested = config.DefaultProvider
		}
		code := "literouter.provider_unknown"
		return core.AttemptOutcome{Error: model.NormalizedError{
			Kind:    model.FailureProvider,
			Label:   "提供商不存在",
			Code:    &code,
			Message: fmt.Sprintf("模型没有可用的提供商配置（providerId: %s）", requested),
			Fatal:   true,
		}}
	}

	key, ok := ResolveAPIKey(provider)
	if !ok {
		code := "literouter.no_key"
		envName := provider.APIKeyEnv
		if envName == "" {
			envName = "（未配置名称）"
		}
		return core.AttemptOutcome{Error: model.NormalizedError{
			Kind:    model.FailureAuth,
			Label:   "未配置 API Key",
			Code:    &code,
			Message: fmt.Sprintf("提供商 %s 没有 Key：环境变量 %s 与 providers[].apiKey 均为空。", provider.ID, envName),
			Fatal:   true,
		}}
	}

	url := strings.TrimRight(provider.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
	payload := make(map[string]any, len(body)+1)
	for key, value := range body {
		payload[key] = value
	}
	payload["model"] = modelCode
	encoded, err := json.Marshal(payload)
	if err != nil {
		return core.AttemptOutcome{Error: core.ClassifyNetworkError(err, false, false)}
	}

	timeoutMs := options.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = provider.RequestTimeoutMs
	}
	started := time.Now()
	requestCtx := ctx
	cancel := func() {}
	var timer *time.Timer
	var timedOut atomic.Bool
	if timeoutMs > 0 {
		var requestCancel context.CancelFunc
		requestCtx, requestCancel = context.WithCancel(ctx)
		cancel = requestCancel
		timer = time.AfterFunc(time.Duration(timeoutMs)*time.Millisecond, func() {
			timedOut.Store(true)
			requestCancel()
		})
	}
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
		}
	}
	release := func() {
		stopTimer()
		cancel()
	}

	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		release()
		return core.AttemptOutcome{Error: core.ClassifyNetworkError(err, false, false)}
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("accept", "application/json")
	request.Header.Set("authorization", "Bearer "+key)
	request.Header.Set("user-agent", "literouter/"+Version)

	response, err := c.httpClient().Do(request)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		release()
		clientAborted := ctx.Err() != nil
		timedOutValue := timedOut.Load() && !clientAborted
		return core.AttemptOutcome{LatencyMs: latency, Error: core.ClassifyNetworkError(err, timedOutValue, clientAborted)}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		// 超时只覆盖等待响应头；拿到响应后取消定时器，长 SSE 不能被超时打断。
		// Body 关闭时释放请求 context。
		stopTimer()
		response.Body = &releaseOnClose{ReadCloser: response.Body, release: cancel}
		return core.AttemptOutcome{OK: true, Response: response, LatencyMs: latency, ProviderID: provider.ID}
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	_ = response.Body.Close()
	release()
	if readErr != nil {
		raw = nil
	}
	return core.AttemptOutcome{
		LatencyMs: latency,
		Error:     core.ClassifyHTTPError(response.StatusCode, core.ParseErrorPayload(string(raw))),
	}
}

type releaseOnClose struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (r *releaseOnClose) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)
	return err
}

// ListModels fetches a provider's model catalogue.
func (c *Client) ListModels(ctx context.Context, providerID string) ([]string, error) {
	config := c.getConfig()
	provider := ResolveProvider(&config, providerID)
	if provider == nil {
		requested := providerID
		if requested == "" {
			requested = config.DefaultProvider
		}
		return nil, fmt.Errorf("提供商不存在：%s", requested)
	}
	key, ok := ResolveAPIKey(provider)
	if !ok {
		return nil, fmt.Errorf("提供商 %s 未配置 API Key", provider.ID)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, strings.TrimRight(provider.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("authorization", "Bearer "+key)
	request.Header.Set("user-agent", "literouter/"+Version)
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
		payload := core.ParseErrorPayload(string(raw))
		code := ""
		if payload.Code != nil {
			code = *payload.Code
		}
		return nil, fmt.Errorf("%d %s %s", response.StatusCode, code, payload.Message)
	}
	decoder := json.NewDecoder(response.Body)
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := decoder.Decode(&data); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("提供商返回了非预期的模型目录")
	}
	ids := make([]string, 0, len(data.Data))
	seen := map[string]bool{}
	for _, item := range data.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}
