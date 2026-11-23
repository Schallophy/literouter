// Package model holds the shared domain types: configuration schema, persisted
// state records and request outcomes. Field names mirror the historical
// state.json layout so existing data directories stay readable.
package model

// Health is the persisted health state machine value.
type Health string

const (
	HealthUnknown Health = "unknown"
	HealthAlive   Health = "alive"
	HealthDead    Health = "dead"
	HealthInvalid Health = "invalid"
)

// ApiKind identifies the OpenAI protocol a request used.
type ApiKind string

const (
	ApiChat        ApiKind = "chat"
	ApiCompletions ApiKind = "completions"
	ApiResponses   ApiKind = "responses"
	ApiEmbeddings  ApiKind = "embeddings"
)

// ApiKinds is the canonical order used in configuration and the dashboard.
var ApiKinds = []ApiKind{ApiChat, ApiCompletions, ApiResponses, ApiEmbeddings}

// DefaultModelApis is what a model supports when the configuration says nothing.
var DefaultModelApis = []ApiKind{ApiChat}

func IsApiKind(value string) bool {
	for _, kind := range ApiKinds {
		if string(kind) == value {
			return true
		}
	}
	return false
}

// Usage is a provider-reported token usage block.
type Usage struct {
	PromptTokens       int  `json:"promptTokens"`
	CompletionTokens   int  `json:"completionTokens"`
	TotalTokens        int  `json:"totalTokens"`
	CachedPromptTokens int  `json:"cachedPromptTokens"`
	CacheReported      bool `json:"cacheReported"`
}

// CostSplit breaks one request's cost into its three token classes.
type CostSplit struct {
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cachedInput"`
	Output      float64 `json:"output"`
	CacheSaved  float64 `json:"cacheSaved"`
}

// CostBreakdown is a CostSplit that may contain estimated history.
type CostBreakdown struct {
	CostSplit
	Estimated bool `json:"estimated"`
}

// Total returns the summed cost of the three billed classes.
func (c CostSplit) Total() float64 { return c.Input + c.CachedInput + c.Output }

// ModelRecord is one model's persisted health and usage counters.
type ModelRecord struct {
	Code                     string        `json:"code"`
	Family                   string        `json:"family"`
	Health                   Health        `json:"health"`
	Reason                   *string       `json:"reason"`
	LastErrorCode            *string       `json:"lastErrorCode"`
	LastStatusCode           *int          `json:"lastStatusCode"`
	LastErrorMessage         *string       `json:"lastErrorMessage"`
	CheckedAt                *string       `json:"checkedAt"`
	LastUsedAt               *string       `json:"lastUsedAt"`
	PromptTokens             int           `json:"promptTokens"`
	CachedPromptTokens       int           `json:"cachedPromptTokens"`
	CacheTrackedPromptTokens int           `json:"cacheTrackedPromptTokens"`
	CompletionTokens         int           `json:"completionTokens"`
	Cost                     float64       `json:"cost"`
	CostBreakdown            CostBreakdown `json:"costBreakdown"`
	Requests                 int           `json:"requests"`
	Failures                 int           `json:"failures"`
	ThrottledUntil           *string       `json:"throttledUntil"`
}

// FailureKind classifies an upstream attempt failure.
type FailureKind string

const (
	FailureQuotaExhausted FailureKind = "quota_exhausted"
	FailureModelInvalid   FailureKind = "model_invalid"
	FailureThrottled      FailureKind = "throttled"
	FailureAuth           FailureKind = "auth"
	FailureForbidden      FailureKind = "forbidden"
	FailureBadRequest     FailureKind = "bad_request"
	FailureProvider       FailureKind = "provider"
	FailureNetwork        FailureKind = "network"
	FailureTimeout        FailureKind = "timeout"
	FailureAborted        FailureKind = "aborted"
	FailureExhausted      FailureKind = "exhausted"
	FailureContext        FailureKind = "context"
	FailureUnknown        FailureKind = "unknown"
)

// NormalizedError is the gateway's uniform view of an attempt failure.
type NormalizedError struct {
	Kind        FailureKind `json:"kind"`
	Label       string      `json:"label"`
	Status      *int        `json:"status"`
	Code        *string     `json:"code"`
	Message     string      `json:"message"`
	Fatal       bool        `json:"fatal"`
	MarkDead    bool        `json:"markDead"`
	MarkInvalid bool        `json:"markInvalid"`
	Retryable   bool        `json:"retryable"`
}

// AttemptRecord is the per-attempt trace exposed in events and dashboards.
type AttemptRecord struct {
	Model     string      `json:"model"`
	OK        bool        `json:"ok"`
	LatencyMs int64       `json:"latencyMs"`
	Kind      FailureKind `json:"kind,omitempty"`
	Status    *int        `json:"status"`
	Code      *string     `json:"code"`
	Message   *string     `json:"message"`
}

// CallerIdentity is the derived client identity used for per-caller stats.
type CallerIdentity struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

var (
	AnonymousCaller = CallerIdentity{ID: "anonymous", Label: "匿名"}
	ConsoleCaller   = CallerIdentity{ID: "console", Label: "看板自测"}
)

// ContextOverflowMode controls what happens when the prompt exceeds the window.
type ContextOverflowMode string

const (
	OverflowReject      ContextOverflowMode = "reject"
	OverflowPassthrough ContextOverflowMode = "passthrough"
)

// ContextConfig is the shared context window guard.
type ContextConfig struct {
	Window     int                 `json:"window"`
	OnOverflow ContextOverflowMode `json:"onOverflow"`
}

// ChainConfig is the single prioritized model list.
type ChainConfig struct {
	Models                 []string      `json:"models"`
	Context                ContextConfig `json:"context"`
	RetryDeadAfterMinutes  int           `json:"retryDeadAfterMinutes"`
	// Disabled models keep their list position and stats, but auto never
	// tries them. Explicit requests to the model still go through.
	Disabled []string `json:"disabled,omitempty"`
}

// ServerConfig holds HTTP server limits.
type ServerConfig struct {
	MaxBodyBytes int  `json:"maxBodyBytes"`
	Cors         bool `json:"cors"`
}

// UiConfig toggles the embedded dashboard.
type UiConfig struct {
	Enabled bool `json:"enabled"`
}

// LogLevelName is a log threshold.
type LogLevelName string

const (
	LogDebug LogLevelName = "debug"
	LogInfo  LogLevelName = "info"
	LogWarn  LogLevelName = "warn"
	LogError LogLevelName = "error"
)

func IsLogLevel(value string) bool {
	switch LogLevelName(value) {
	case LogDebug, LogInfo, LogWarn, LogError:
		return true
	}
	return false
}

// LogConfig controls file logging and rotation.
type LogConfig struct {
	Level    LogLevelName `json:"level"`
	File     string       `json:"file"`
	MaxBytes int64        `json:"maxBytes"`
	MaxFiles int          `json:"maxFiles"`
}

// ProviderConfig is one upstream OpenAI-compatible endpoint.
type ProviderConfig struct {
	ID                string `json:"id"`
	Label             string `json:"label"`
	BaseURL           string `json:"baseUrl"`
	APIKeyEnv         string `json:"apiKeyEnv"`
	APIKey            string `json:"apiKey"`
	RequestTimeoutMs  int    `json:"requestTimeoutMs"`
	InjectStreamUsage bool   `json:"injectStreamUsage"`
}

// FailoverConfig bounds the failover loop.
type FailoverConfig struct {
	MaxAttempts        int `json:"maxAttempts"`
	ThrottleCooldownMs int `json:"throttleCooldownMs"`
}

// PriceTier is one time-of-day price band, priced per PricingConfig.Unit tokens.
type PriceTier struct {
	From        string  `json:"from"`
	To          string  `json:"to"`
	Input       float64 `json:"input"`
	CachedInput float64 `json:"cachedInput"`
	Output      float64 `json:"output"`
}

// ModelPricing gives a model its own timezone and price bands.
type ModelPricing struct {
	Timezone string      `json:"timezone"`
	Tiers    []PriceTier `json:"tiers"`
}

// PricingConfig is the pricing catalogue. The "*" key is the fallback entry.
// PreferOffPeak reorders auto candidates by their current time-of-day band.
type PricingConfig struct {
	Currency      string                  `json:"currency"`
	Unit          int64                   `json:"unit"`
	PreferOffPeak bool                    `json:"preferOffPeak"`
	Models        map[string]ModelPricing `json:"models"`
}

// ResponsesChainingConfig toggles platform-side response session reuse.
type ResponsesChainingConfig struct {
	Enabled     bool `json:"enabled"`
	TTLMinutes  int  `json:"ttlMinutes"`
	MaxSessions int  `json:"maxSessions"`
}

// GatewayConfig is the whole config.json document.
type GatewayConfig struct {
	Version           int                      `json:"version"`
	Host              string                   `json:"host"`
	Port              int                      `json:"port"`
	GatewayToken      string                   `json:"gatewayToken"`
	Server            ServerConfig             `json:"server"`
	UI                UiConfig                 `json:"ui"`
	Log               LogConfig                `json:"log"`
	Chain             ChainConfig              `json:"chain"`
	ModelApis         map[string][]ApiKind     `json:"modelApis"`
	Providers         []ProviderConfig         `json:"providers"`
	DefaultProvider   string                   `json:"defaultProvider"`
	Failover          FailoverConfig           `json:"failover"`
	Pricing           PricingConfig            `json:"pricing"`
	ResponsesChaining ResponsesChainingConfig  `json:"responsesChaining"`
}

// ProviderByID returns the provider with the given id.
func (c *GatewayConfig) ProviderByID(id string) *ProviderConfig {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return &c.Providers[i]
		}
	}
	return nil
}
