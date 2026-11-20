// Package runtime orchestrates routing, failover, responses chaining and
// configuration updates.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Schallophy/literouter/internal/config"
	"github.com/Schallophy/literouter/internal/core"
	"github.com/Schallophy/literouter/internal/model"
	"github.com/Schallophy/literouter/internal/provider"
	"github.com/Schallophy/literouter/internal/store"
	"github.com/Schallophy/literouter/internal/stream"
	"github.com/Schallophy/literouter/internal/util"
)

var failureLabels = map[model.FailureKind]string{
	model.FailureQuotaExhausted: "额度耗尽",
	model.FailureModelInvalid:   "模型不存在",
	model.FailureThrottled:      "限流中",
	model.FailureAuth:           "认证失败",
	model.FailureForbidden:      "无权限",
	model.FailureBadRequest:     "参数错误",
	model.FailureProvider:       "上游故障",
	model.FailureNetwork:        "网络错误",
	model.FailureTimeout:        "超时",
	model.FailureAborted:        "已中断",
	model.FailureExhausted:      "全部不可用",
	model.FailureContext:        "超出上下文",
	model.FailureUnknown:        "未知错误",
}

// Options builds a Runtime.
type Options struct {
	ConfigPath string
	DataDir    string
	Config     model.GatewayConfig
	Logger     *util.Logger
	Warnings   []string
}

// SnapshotModel is a model record with routing metadata.
type SnapshotModel struct {
	model.ModelRecord
	APIs     []model.ApiKind `json:"apis"`
	Provider string          `json:"provider"`
	Upstream string          `json:"upstream"`
}

// ChainSnapshot is what the dashboard's model page renders.
type ChainSnapshot struct {
	Effective             *string         `json:"effective"`
	Context               model.ContextConfig `json:"context"`
	RetryDeadAfterMinutes int             `json:"retryDeadAfterMinutes"`
	Models                []SnapshotModel `json:"models"`
	Alive                 int             `json:"alive"`
	Dead                  int             `json:"dead"`
}

// OutputSnapshot follows the model that actually served the last request.
type OutputSnapshot struct {
	Model     string          `json:"model"`
	Provider  *string         `json:"provider"`
	API       model.ApiKind   `json:"api"`
	Requested string          `json:"requested"`
	Mode      core.RouteMode  `json:"mode"`
	At        string          `json:"at"`
}

// ConfigRuleError is a user-facing configuration conflict (HTTP 400).
type ConfigRuleError struct {
	Message string
	Details []string
}

func (e *ConfigRuleError) Error() string { return e.Message }

// TestResult is the dashboard self-test result.
type TestResult struct {
	OK        bool                 `json:"ok"`
	Model     *string              `json:"model"`
	Content   *string              `json:"content"`
	LatencyMs int64                `json:"latencyMs"`
	Usage     *model.Usage         `json:"usage"`
	Attempts  []model.AttemptRecord `json:"attempts"`
	Message   *string              `json:"message"`
}

// CalibrationSample captures the local prompt estimate for learning.
type CalibrationSample struct {
	EstimatedPromptTokens int  `json:"estimatedPromptTokens"`
	HasTools              bool `json:"hasTools"`
}

// RoutedResult is the failover outcome plus calibration input.
type RoutedResult struct {
	Result core.FailoverResult
	Sample *CalibrationSample
}

// RequestInput drives RequestThrough.
type RequestInput struct {
	Path        string
	Model       string
	Body        map[string]any
	Context     context.Context
	MaxAttempts int
	Caller      model.CallerIdentity
}

// ChainStats reports responses chaining effectiveness.
type ChainStats struct {
	Enabled   bool `json:"enabled"`
	Requests  int  `json:"requests"`
	Hits      int  `json:"hits"`
	Fallbacks int  `json:"fallbacks"`
	Sessions  int  `json:"sessions"`
}

// Runtime is the process-wide gateway engine.
type Runtime struct {
	configMu   sync.RWMutex
	config     model.GatewayConfig
	configPath string
	dataDir    string
	Logger     *util.Logger
	Store      *store.StateStore
	Events     *stream.EventLog
	LiveOutput *stream.LiveOutputLog
	Client     *provider.Client
	capability *core.CapabilityService

	bootedAt time.Time

	warningsMu   sync.Mutex
	warnings     []string
	catalogError string

	chainMu        sync.Mutex
	chainStore     *stream.ResponseChainStore
	chainRequests  int
	chainHits      int
	chainFallbacks int

	outputMu sync.Mutex
	output   *OutputSnapshot
}

// Create opens persisted state and prepares migrations.
func Create(options Options) (*Runtime, error) {
	logger := options.Logger
	if logger == nil {
		logger = util.NopLogger()
	}
	state, err := store.Open(store.OpenOptions{
		File:  options.DataDir + string(os.PathSeparator) + "state.json",
		Codes: collectCodes(options.Config, nil),
		OnError: func(err error) {
			logger.Warn("state persistence failed", map[string]any{"error": err.Error()})
		},
	})
	if err != nil {
		return nil, err
	}
	rt := &Runtime{
		config:     options.Config,
		configPath: options.ConfigPath,
		dataDir:    options.DataDir,
		Logger:     logger,
		Store:      state,
		Events:     stream.NewEventLog(options.DataDir+string(os.PathSeparator)+"events.jsonl", 400),
		LiveOutput: stream.NewLiveOutputLog(0, 0, 0),
		capability: core.NewCapabilityService(nil),
		bootedAt:   time.Now(),
		warnings:   append([]string(nil), options.Warnings...),
		chainStore: stream.NewResponseChainStore(),
	}
	rt.Client = provider.NewClient(rt.ConfigSnapshot)
	aliases := collectModelAliases(options.Config)
	if len(aliases) > 0 {
		state.MigrateModelCodes(func(code string) string { return aliases[code] })
	}
	state.PruneModels(options.Config.Chain.Models)

	at := time.Now()
	filled := state.BackfillUsageDetails(func(modelCode string, usage store.UsageTokens) *model.CostSplit {
		if modelCode == "" {
			return nil
		}
		split, ok := core.CostForUsage(options.Config.Pricing, modelCode, modelTokenUsage(usage), at)
		if !ok {
			return nil
		}
		return &split
	})
	if filled {
		logger.Info("已补齐历史用量的成本拆分与模型明细", nil)
	}
	return rt, nil
}

func modelTokenUsage(usage store.UsageTokens) model.Usage {
	return model.Usage{PromptTokens: usage.PromptTokens, CachedPromptTokens: usage.CachedPromptTokens, CompletionTokens: usage.CompletionTokens}
}

// ConfigSnapshot returns a deep copy safe for request-local use.
func (r *Runtime) ConfigSnapshot() model.GatewayConfig {
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	return cloneConfig(r.config)
}

func cloneConfig(cfg model.GatewayConfig) model.GatewayConfig {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return cfg
	}
	var copied model.GatewayConfig
	if err := json.Unmarshal(encoded, &copied); err != nil {
		return cfg
	}
	return copied
}

// BootedAt returns process start time.
func (r *Runtime) BootedAt() time.Time { return r.bootedAt }

// ConfigPath is the absolute or configured config file location.
func (r *Runtime) ConfigPath() string { return r.configPath }

// DataDir is the directory holding state.json and events.jsonl.
func (r *Runtime) DataDir() string { return r.dataDir }

// Capability exposes the leaderboard service.
func (r *Runtime) Capability() *core.CapabilityService { return r.capability }

// Warnings returns load warnings plus a catalog failure.
func (r *Runtime) Warnings() []string {
	r.warningsMu.Lock()
	defer r.warningsMu.Unlock()
	list := append([]string(nil), r.warnings...)
	if r.catalogError != "" {
		list = append(list, "模型目录刷新失败："+r.catalogError)
	}
	return list
}

// KeyInfo reports the default provider's key.
func (r *Runtime) KeyInfo() provider.KeyInfo {
	cfg := r.ConfigSnapshot()
	return provider.ResolveKeyInfo(&cfg)
}

// ProviderKeyInfos reports every provider's key state.
func (r *Runtime) ProviderKeyInfos() []provider.KeyInfo {
	cfg := r.ConfigSnapshot()
	infos := make([]provider.KeyInfo, 0, len(cfg.Providers))
	for i := range cfg.Providers {
		infos = append(infos, provider.KeyInfoFor(&cfg.Providers[i]))
	}
	return infos
}

// ProviderFor resolves the provider bound to a model code.
func (r *Runtime) ProviderFor(code string) *model.ProviderConfig {
	cfg := r.ConfigSnapshot()
	return providerFor(&cfg, code)
}

func providerFor(cfg *model.GatewayConfig, code string) *model.ProviderConfig {
	if parsed, ok := core.SplitModelCode(code, cfg.Providers); ok {
		return provider.ResolveProvider(cfg, parsed.Provider)
	}
	return provider.ResolveProvider(cfg, "")
}

// ProviderIDFor returns the provider id or the default.
func (r *Runtime) ProviderIDFor(code string) string {
	cfg := r.ConfigSnapshot()
	if providerCfg := providerFor(&cfg, code); providerCfg != nil {
		return providerCfg.ID
	}
	return cfg.DefaultProvider
}

// UpstreamModelFor strips the provider prefix.
func (r *Runtime) UpstreamModelFor(code string) string {
	cfg := r.ConfigSnapshot()
	if parsed, ok := core.SplitModelCode(code, cfg.Providers); ok {
		return parsed.Upstream
	}
	return code
}

// NormalizeModelCode canonicalizes "provider/upstream".
func (r *Runtime) NormalizeModelCode(raw string) (string, bool) {
	cfg := r.ConfigSnapshot()
	parsed, ok := core.SplitModelCode(strings.TrimSpace(raw), cfg.Providers)
	if !ok {
		return "", false
	}
	return core.JoinModelCode(parsed.Provider, parsed.Upstream), true
}

// ApisFor returns a model's declared APIs.
func (r *Runtime) ApisFor(code string) []model.ApiKind {
	cfg := r.ConfigSnapshot()
	return core.ApisFor(cfg.ModelApis, code)
}

// SupportsAPI reports whether a model declares the protocol.
func (r *Runtime) SupportsAPI(code string, api model.ApiKind) bool {
	cfg := r.ConfigSnapshot()
	return core.SupportsAPI(cfg.ModelApis, code, api)
}

// PrimaryModel is the head of the chain.
func (r *Runtime) PrimaryModel() *string {
	cfg := r.ConfigSnapshot()
	if len(cfg.Chain.Models) == 0 {
		return nil
	}
	code := cfg.Chain.Models[0]
	return &code
}

// ChainCodes returns a copy of the chain order.
func (r *Runtime) ChainCodes() []string {
	cfg := r.ConfigSnapshot()
	return append([]string(nil), cfg.Chain.Models...)
}

// EffectiveModel is what auto would hit right now.
func (r *Runtime) EffectiveModel() *string {
	cfg := r.ConfigSnapshot()
	retryMs := int64(cfg.Chain.RetryDeadAfterMinutes) * 60_000
	now := time.Now().UnixMilli()
	for _, code := range cfg.Chain.Models {
		record := r.Store.Get(code)
		health := model.HealthUnknown
		if record != nil {
			health = record.Health
		}
		if health == model.HealthInvalid {
			continue
		}
		if health == model.HealthDead && (record == nil || !core.IsDeadRetryDue(record, now, retryMs)) {
			continue
		}
		if record != nil && core.IsThrottled(record, now) {
			continue
		}
		found := code
		return &found
	}
	return nil
}

// LastOutput returns the last served model.
func (r *Runtime) LastOutput() *OutputSnapshot {
	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	if r.output == nil {
		return nil
	}
	copied := *r.output
	return &copied
}

func (r *Runtime) setLastOutput(value OutputSnapshot) {
	r.outputMu.Lock()
	r.output = &value
	r.outputMu.Unlock()
}

// SnapshotModel builds one dashboard model row.
func (r *Runtime) SnapshotModel(code string) SnapshotModel {
	cfg := r.ConfigSnapshot()
	record := r.Store.Get(code)
	if record == nil {
		record = &model.ModelRecord{Code: code, Family: "Other", Health: model.HealthUnknown, CostBreakdown: model.CostBreakdown{}}
	}
	upstream := code
	if parsed, ok := core.SplitModelCode(code, cfg.Providers); ok {
		upstream = parsed.Upstream
	}
	family := util.FamilyOf(upstream)
	if family == "Other" {
		family = record.Family
	}
	providerID := cfg.DefaultProvider
	if providerCfg := providerFor(&cfg, code); providerCfg != nil {
		providerID = providerCfg.ID
	}
	copied := *record
	copied.Family = family
	return SnapshotModel{
		ModelRecord: copied,
		APIs:        core.ApisFor(cfg.ModelApis, code),
		Provider:    providerID,
		Upstream:    upstream,
	}
}

// ChainSnapshot builds the model page payload.
func (r *Runtime) ChainSnapshot() ChainSnapshot {
	cfg := r.ConfigSnapshot()
	models := make([]SnapshotModel, 0, len(cfg.Chain.Models))
	alive, dead := 0, 0
	for _, code := range cfg.Chain.Models {
		snapshot := r.SnapshotModel(code)
		models = append(models, snapshot)
		switch snapshot.Health {
		case model.HealthAlive:
			alive++
		case model.HealthDead, model.HealthInvalid:
			dead++
		}
	}
	return ChainSnapshot{
		Effective:             r.EffectiveModel(),
		Context:               cfg.Chain.Context,
		RetryDeadAfterMinutes: cfg.Chain.RetryDeadAfterMinutes,
		Models:                models,
		Alive:                 alive,
		Dead:                  dead,
	}
}

// Hooks wires failover callbacks to health updates.
func (r *Runtime) Hooks() core.FailoverHooks {
	return core.FailoverHooks{
		OnDead:     r.MarkDead,
		OnInvalid:  r.MarkInvalid,
		OnThrottle: r.MarkThrottled,
		OnSkip:     r.MarkSkipped,
	}
}

// MarkDead persists a quota-exhausted model.
func (r *Runtime) MarkDead(modelCode string, failure model.NormalizedError) {
	now := isoTimestamp()
	r.Store.SetHealth(modelCode, model.HealthDead, map[string]any{
		"reason":           failure.Label,
		"lastErrorCode":    failure.Code,
		"lastStatusCode":   failure.Status,
		"lastErrorMessage": failure.Message,
		"checkedAt":        now,
	})
	r.Events.Push(stream.EventInput{
		Kind: stream.EventDead, Level: stream.LevelWarn,
		Message: fmt.Sprintf("%s 已标记为不可用（%s）", modelCode, failure.Label),
		Model:   &modelCode,
		Detail:  map[string]any{"status": failure.Status, "code": failure.Code, "message": failure.Message},
	})
	fields := map[string]any{"code": failure.Code}
	if failure.Status != nil {
		fields["status"] = *failure.Status
	}
	r.Logger.Warn(modelCode+" 不可用："+failure.Label, fields)
}

// MarkInvalid persists a model the provider no longer serves.
func (r *Runtime) MarkInvalid(modelCode string, failure model.NormalizedError) {
	now := isoTimestamp()
	r.Store.SetHealth(modelCode, model.HealthInvalid, map[string]any{
		"reason":           failure.Label,
		"lastErrorCode":    failure.Code,
		"lastStatusCode":   failure.Status,
		"lastErrorMessage": failure.Message,
		"checkedAt":        now,
	})
	r.Events.Push(stream.EventInput{
		Kind: stream.EventDead, Level: stream.LevelWarn,
		Message: fmt.Sprintf("%s 模型不可用（%s）", modelCode, failure.Label),
		Model:   &modelCode,
		Detail:  map[string]any{"status": failure.Status, "code": failure.Code},
	})
	r.Logger.Warn(modelCode+"："+failure.Label, map[string]any{"code": failure.Code})
}

// MarkThrottled starts a cooldown without changing health.
func (r *Runtime) MarkThrottled(modelCode string, failure model.NormalizedError) {
	cfg := r.ConfigSnapshot()
	until := time.Now().Add(time.Duration(cfg.Failover.ThrottleCooldownMs) * time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
	health := model.HealthUnknown
	if record := r.Store.Get(modelCode); record != nil {
		health = record.Health
	}
	r.Store.SetHealth(modelCode, health, map[string]any{
		"throttledUntil":   until,
		"lastErrorCode":    failure.Code,
		"lastStatusCode":   failure.Status,
		"lastErrorMessage": failure.Message,
	})
	r.Events.Push(stream.EventInput{
		Kind: stream.EventThrottle, Level: stream.LevelInfo,
		Message: fmt.Sprintf("%s 触发限流，冷却至 %s", modelCode, until),
		Model:   &modelCode,
		Detail:  map[string]any{"status": failure.Status, "code": failure.Code},
	})
}

// MarkSkipped re-arms a dead model that failed its automatic retry.
func (r *Runtime) MarkSkipped(modelCode string, failure model.NormalizedError) {
	skipFields := map[string]any{"model": modelCode, "kind": failure.Kind}
	if failure.Status != nil {
		skipFields["status"] = *failure.Status
	}
	r.Logger.Debug("attempt skipped", skipFields)
	record := r.Store.Get(modelCode)
	if record != nil && record.Health == model.HealthDead {
		r.Store.SetHealth(modelCode, model.HealthDead, map[string]any{
			"lastErrorCode":    failure.Code,
			"lastStatusCode":   failure.Status,
			"lastErrorMessage": failure.Message,
			"checkedAt":        isoTimestamp(),
		})
	}
}

func rejectResult(err model.NormalizedError, sample *CalibrationSample) RoutedResult {
	return RoutedResult{
		Result: core.FailoverResult{Error: err, Attempts: []model.AttemptRecord{}},
		Sample: sample,
	}
}

// RequestThrough resolves the route and runs the failover loop.
func (r *Runtime) RequestThrough(input RequestInput) RoutedResult {
	cfg := r.ConfigSnapshot()
	api := core.APIKindForPath(input.Path)
	route := core.ResolveRoute(input.Model, cfg.Chain, cfg.Providers)

	hasTools := isNonEmptyArray(input.Body["tools"]) || isNonEmptyArray(input.Body["functions"])
	var estimate *util.ContextEstimate
	var sample *CalibrationSample
	if isChatPath(input.Path) {
		value := util.EstimateRequestTokens(input.Path, input.Body)
		estimate = &value
		sample = &CalibrationSample{EstimatedPromptTokens: value.PromptTokens, HasTools: hasTools}
	}

	apiError := func(message string) RoutedResult {
		code := "literouter.api_unsupported"
		status := 400
		return rejectResult(model.NormalizedError{
			Kind: model.FailureBadRequest, Label: "API 不兼容", Status: &status, Code: &code,
			Message: message, Fatal: true,
		}, sample)
	}

	if route.Mode == core.RouteAuto && len(cfg.Chain.Models) == 0 {
		code := "literouter.chain_empty"
		status := 503
		return rejectResult(model.NormalizedError{
			Kind: model.FailureExhausted, Label: "模型列表为空", Status: &status, Code: &code,
			Message: "模型列表里还没有模型。请在看板的「模型」页加入模型，或在 config.json 的 chain.models 中填写。",
			Fatal: true,
		}, sample)
	}
	if route.Model != "" && route.Mode != core.RoutePassthrough && !core.SupportsAPI(cfg.ModelApis, route.Model, api) {
		return apiError(fmt.Sprintf("模型 %s 未声明支持 %s API。请在该模型的 API 能力中启用 %s，或改用 auto。", route.Model, api, api))
	}
	if route.Mode == core.RouteAuto {
		supported := false
		for _, code := range cfg.Chain.Models {
			if core.SupportsAPI(cfg.ModelApis, code, api) {
				supported = true
				break
			}
		}
		if !supported {
			return apiError(fmt.Sprintf("模型列表中没有声明支持 %s API 的模型。请在配置 modelApis 中为候选模型启用 %s。", api, api))
		}
	}
	if route.Mode == core.RoutePassthrough && route.Model != "" {
		if _, declared := cfg.ModelApis[route.Model]; declared && !core.SupportsAPI(cfg.ModelApis, route.Model, api) {
			return apiError(fmt.Sprintf("模型 %s 未声明支持 %s API。", route.Model, api))
		}
	}

	var candidates []string
	if route.Mode == core.RouteAuto {
		now := time.Now().UnixMilli()
		selected := core.SelectCandidates(core.SelectionInput{
			Chain:     cfg.Chain,
			GetRecord: r.Store.Get,
			Now:       now,
			Accepts: func(code string) bool {
				return core.SupportsAPI(cfg.ModelApis, code, api)
			},
			RetryDeadAfterMs: int64(cfg.Chain.RetryDeadAfterMinutes) * 60_000,
		})
		candidates = make([]string, 0, len(selected))
		for _, candidate := range selected {
			candidates = append(candidates, candidate.Code)
		}
	} else if route.Model != "" {
		candidates = []string{route.Model}
	}

	if estimate != nil && cfg.Chain.Context.OnOverflow == model.OverflowReject && len(candidates) > 0 {
		modelCode := candidates[0]
		ratio := r.Store.CalibrationRatio(modelCode, hasTools)
		inputTokens := int(math.Round(float64(estimate.PromptTokens) * ratio))
		totalTokens := inputTokens + estimate.OutputTokens
		if totalTokens > cfg.Chain.Context.Window {
			calibration := ""
			if ratio != 1 {
				calibration = fmt.Sprintf("，按 %s 的校准系数 ×%.2f（原始估算 %d）", modelCode, ratio, estimate.PromptTokens)
			}
			code := "literouter.context_length_exceeded"
			status := 400
			message := fmt.Sprintf("请求约 %d tokens（输入 %d + 输出 %d%s），超过模型列表的上下文窗口 %d。请缩短输入、调小 max_tokens，或调整 chain.context.window。",
				totalTokens, inputTokens, estimate.OutputTokens, calibration, cfg.Chain.Context.Window)
			return rejectResult(model.NormalizedError{
				Kind: model.FailureContext, Label: "上下文超限", Status: &status, Code: &code,
				Message: message, Fatal: true,
			}, sample)
		}
	}

	chaining := cfg.ResponsesChaining
	chainEnabled := chaining.Enabled && input.Path == "responses" && len(candidates) > 0 && input.Body["previous_response_id"] == nil

	forwardBody := input.Body
	var chainItems []any
	var chainHitKey string
	if chainEnabled {
		items := stream.ResponsesInputItems(input.Body["input"])
		if items != nil {
			caller := input.Caller
			if caller.ID == "" {
				caller = model.AnonymousCaller
			}
			scope := stream.ChainScope(caller.ID, providerIDForConfig(&cfg, candidates[0]), candidates[0], input.Body["tools"])
			hashes := stream.ItemsPrefixHashes(scope, items)
			for index := len(hashes) - 1; index >= 0; index-- {
				key := hashes[index]
				id := r.chainStore.Lookup(key, time.Now().UnixMilli())
				if id == "" {
					continue
				}
				delta := stream.ChainDelta(items, index+1)
				if delta == nil {
					continue
				}
				forwardBody = copyBody(input.Body)
				forwardBody["input"] = delta
				forwardBody["previous_response_id"] = id
				forwardBody["store"] = true
				chainHitKey = key
				break
			}
			if chainHitKey == "" {
				forwardBody = copyBody(input.Body)
				forwardBody["store"] = true
			}
			chainItems = items
			r.chainMu.Lock()
			r.chainRequests++
			if chainHitKey != "" {
				r.chainHits++
			}
			r.chainMu.Unlock()
		}
	}

	requestContext := input.Context
	if requestContext == nil {
		requestContext = context.Background()
	}
	maxAttempts := input.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = cfg.Failover.MaxAttempts
	}
	attemptWith := func(body map[string]any) core.Attemptor {
		return func(ctx context.Context, modelCode string) core.AttemptOutcome {
			upstream := modelCode
			if parsed, ok := core.SplitModelCode(modelCode, cfg.Providers); ok {
				upstream = parsed.Upstream
			}
			return r.Client.Attempt(ctx, input.Path, upstream, body, provider.AttemptOptions{ProviderID: providerIDForConfig(&cfg, modelCode)})
		}
	}

	result := core.RunFailover(core.FailoverInput{
		Context:     requestContext,
		Candidates:  candidates,
		MaxAttempts: maxAttempts,
		Attempt:     attemptWith(forwardBody),
		Hooks:       r.Hooks(),
	})
	chainServed := chainHitKey != ""

	if chainHitKey != "" && !result.OK && result.Error.Status != nil && (*result.Error.Status == 400 || *result.Error.Status == 404) {
		r.chainStore.Delete(chainHitKey)
		chainHitKey = ""
		chainServed = false
		r.chainMu.Lock()
		r.chainFallbacks++
		r.chainMu.Unlock()
		retryBody := copyBody(input.Body)
		retryBody["store"] = true
		result = core.RunFailover(core.FailoverInput{
			Context:     requestContext,
			Candidates:  candidates,
			MaxAttempts: maxAttempts,
			Attempt:     attemptWith(retryBody),
			Hooks:       r.Hooks(),
		})
	}

	if chainEnabled && result.OK && chainItems != nil && result.Response != nil {
		providerID := result.Provider
		if providerID == "" {
			providerID = providerIDForConfig(&cfg, result.Model)
		}
		caller := input.Caller
		if caller.ID == "" {
			caller = model.AnonymousCaller
		}
		scope := stream.ChainScope(caller.ID, providerID, result.Model, input.Body["tools"])
		hashes := stream.ItemsPrefixHashes(scope, chainItems)
		if len(hashes) > 0 {
			key := hashes[len(hashes)-1]
			ttl := int64(chaining.TTLMinutes) * 60_000
			maxSessions := chaining.MaxSessions
			result.Response.Body = stream.CaptureResponseID(result.Response.Body, func(id string) {
				r.chainStore.Remember(key, id, ttl, maxSessions, time.Now().UnixMilli())
			})
		}
	}

	if result.OK {
		requested := strings.TrimSpace(input.Model)
		if requested == "" {
			requested = "auto"
		}
		var providerID *string
		if result.Provider != "" {
			value := result.Provider
			providerID = &value
		} else if id := providerIDForConfig(&cfg, result.Model); id != "" {
			value := id
			providerID = &value
		}
		r.setLastOutput(OutputSnapshot{
			Model: result.Model, Provider: providerID, API: api, Requested: requested,
			Mode: route.Mode, At: isoTimestamp(),
		})
	}

	if chainServed {
		return RoutedResult{Result: result, Sample: nil}
	}
	return RoutedResult{Result: result, Sample: sample}
}

func providerIDForConfig(cfg *model.GatewayConfig, code string) string {
	if providerCfg := providerFor(cfg, code); providerCfg != nil {
		return providerCfg.ID
	}
	return cfg.DefaultProvider
}

func copyBody(body map[string]any) map[string]any {
	copied := make(map[string]any, len(body))
	for key, value := range body {
		copied[key] = value
	}
	return copied
}

func isNonEmptyArray(value any) bool {
	list, ok := value.([]any)
	return ok && len(list) > 0
}

// ChainStats reports responses chain effectiveness.
func (r *Runtime) ChainStats() ChainStats {
	cfg := r.ConfigSnapshot()
	r.chainMu.Lock()
	defer r.chainMu.Unlock()
	return ChainStats{
		Enabled:   cfg.ResponsesChaining.Enabled,
		Requests:  r.chainRequests,
		Hits:      r.chainHits,
		Fallbacks: r.chainFallbacks,
		Sessions:  r.chainStore.Size(),
	}
}

// RecordOutcome updates stats, caller counters and events.
func (r *Runtime) RecordOutcome(result core.FailoverResult, caller model.CallerIdentity, api model.ApiKind) {
	if caller.ID == "" {
		caller = model.AnonymousCaller
	}
	r.Store.BumpRequests(api)
	var lastModel *string
	if result.OK {
		lastModel = &result.Model
	}
	r.Store.RecordCallerRequest(caller, result.OK, result.Failovers, lastModel, api)
	for _, attempt := range result.Attempts {
		if !attempt.OK {
			r.Store.MarkFailure(attempt.Model)
			r.Store.MarkCallerModelFailure(caller, attempt.Model, api)
		}
	}

	if result.OK {
		if result.Failovers > 0 {
			r.Store.BumpFailovers(result.Failovers)
			message := "已切换到 " + result.Model
			for _, attempt := range result.Attempts {
				if !attempt.OK {
					label := failureLabels[attempt.Kind]
					if label == "" {
						label = failureLabels[model.FailureUnknown]
					}
					message = fmt.Sprintf("%s（%s）→ %s（切换 %d 次）", attempt.Model, label, result.Model, result.Failovers)
					break
				}
			}
			r.Events.Push(stream.EventInput{
				Kind: stream.EventFailover, Level: stream.LevelWarn, Message: message,
				Model: &result.Model, Detail: map[string]any{"attempts": summarizeAttempts(result.Attempts)},
			})
			r.Logger.Info("failover succeeded", map[string]any{"model": result.Model, "failovers": result.Failovers})
		}
		return
	}

	r.Store.BumpFailovers(result.Failovers)
	message := result.Error.Label + "：" + result.Error.Message
	if result.Exhausted {
		message = "模型列表没有可用的模型"
	}
	r.Events.Push(stream.EventInput{
		Kind: stream.EventError, Level: stream.LevelError, Message: message,
		Detail: map[string]any{
			"attempts": summarizeAttempts(result.Attempts),
			"kind":     result.Error.Kind,
			"code":     result.Error.Code,
		},
	})
	fields := map[string]any{"attempts": len(result.Attempts)}
	if result.Error.Status != nil {
		fields["status"] = *result.Error.Status
	}
	r.Logger.Warn("请求失败："+result.Error.Label, fields)
}

// MarkSuccess persists usage, cost and caller accounting.
func (r *Runtime) MarkSuccess(modelCode string, usage *model.Usage, caller model.CallerIdentity, sample *CalibrationSample, api model.ApiKind) {
	cfg := r.ConfigSnapshot()
	if caller.ID == "" {
		caller = model.AnonymousCaller
	}
	var tier model.PriceTier
	hasTier := false
	if usage != nil {
		tier, hasTier = core.PriceTierAt(cfg.Pricing, modelCode, time.Now())
	}
	var parts *model.CostSplit
	cost := 0.0
	if usage != nil && hasTier {
		split := core.UsageCostParts(*usage, tier, cfg.Pricing.Unit)
		parts = &split
		cost = split.Total()
	}
	r.Store.MarkUsed(modelCode, usage, cost, api, parts)
	r.Store.AddCallerUsage(caller, usage, modelCode, cost, api, parts)
	if usage != nil && sample != nil {
		r.Store.ObserveCalibration(modelCode, sample.HasTools, sample.EstimatedPromptTokens, usage.PromptTokens)
	}
}

// RefreshCatalog fetches provider model lists.
func (r *Runtime) RefreshCatalog(target string) (bool, int, string) {
	cfg := r.ConfigSnapshot()
	targets := cfg.Providers
	if target != "" {
		targets = nil
		for _, p := range cfg.Providers {
			if p.ID == target {
				targets = append(targets, p)
			}
		}
	}
	if len(targets) == 0 {
		return false, 0, "提供商不存在：" + target
	}
	usable := make([]model.ProviderConfig, 0, len(targets))
	for _, p := range targets {
		if provider.KeyInfoFor(&p).Configured {
			usable = append(usable, p)
		}
	}
	if len(usable) == 0 {
		messages := make([]string, 0, len(targets))
		for _, p := range targets {
			messages = append(messages, p.ID+" 未配置 API Key")
		}
		return false, 0, strings.Join(messages, "；")
	}
	type entry struct{ ID, Provider string }
	entries := make([]entry, 0)
	var failures []string
	for _, p := range usable {
		ids, err := r.Client.ListModels(context.Background(), p.ID)
		if err != nil {
			failures = append(failures, p.ID+": "+err.Error())
			continue
		}
		for _, id := range ids {
			entries = append(entries, entry{ID: id, Provider: p.ID})
		}
	}
	if len(entries) == 0 {
		message := strings.Join(failures, "；")
		if message == "" {
			message = "模型目录为空"
		}
		r.setCatalogError(message)
		r.Events.Push(stream.EventInput{Kind: stream.EventCatalog, Level: stream.LevelWarn, Message: "模型目录刷新失败：" + message})
		return false, 0, message
	}
	catalog := make([]struct{ ID, Provider string }, 0, len(entries))
	for _, e := range entries {
		catalog = append(catalog, struct{ ID, Provider string }{ID: e.ID, Provider: e.Provider})
	}
	r.Store.SetCatalog(catalog)
	r.Store.Ensure(cfg.Chain.Models)
	r.Store.PruneModels(cfg.Chain.Models)
	summary := fmt.Sprintf("模型目录已更新，共 %d 个模型（%d 个提供商）", len(entries), len(usable))
	level := stream.LevelInfo
	failureMessage := ""
	if len(failures) > 0 {
		failureMessage = strings.Join(failures, "；")
		summary = fmt.Sprintf("模型目录已更新，共 %d 个模型（部分提供商失败：%s）", len(entries), failureMessage)
		level = stream.LevelWarn
	}
	r.setCatalogError(failureMessage)
	r.Events.Push(stream.EventInput{Kind: stream.EventCatalog, Level: level, Message: summary})
	return true, len(entries), failureMessage
}

func (r *Runtime) setCatalogError(message string) {
	r.warningsMu.Lock()
	r.catalogError = message
	r.warningsMu.Unlock()
}

// Test runs the dashboard self-test prompt.
func (r *Runtime) Test(prompt string) TestResult {
	if r.PrimaryModel() == nil {
		message := "模型列表里还没有模型"
		return TestResult{Message: &message, Attempts: []model.AttemptRecord{}}
	}
	body := map[string]any{
		"messages":  []any{map[string]any{"role": "user", "content": prompt}},
		"stream":    false,
		"max_tokens": 512.0,
	}
	routed := r.RequestThrough(RequestInput{
		Path: "chat/completions", Model: "auto", Body: body,
		Context: context.Background(), Caller: model.ConsoleCaller,
	})
	r.RecordOutcome(routed.Result, model.ConsoleCaller, model.ApiChat)
	if !routed.Result.OK {
		message := routed.Result.Error.Label + "：" + routed.Result.Error.Message
		return TestResult{LatencyMs: routed.Result.LatencyMs, Attempts: routed.Result.Attempts, Message: &message}
	}
	defer routed.Result.Response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(routed.Result.Response.Body, 16*1024*1024))
	if err != nil {
		message := err.Error()
		return TestResult{LatencyMs: routed.Result.LatencyMs, Attempts: routed.Result.Attempts, Message: &message}
	}
	var parsed any
	_ = json.Unmarshal(raw, &parsed)
	usage, _ := util.ExtractUsage(parsed)
	var usagePtr *model.Usage
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.CacheReported {
		usagePtr = &usage
	}
	r.MarkSuccess(routed.Result.Model, usagePtr, model.ConsoleCaller, routed.Sample, model.ApiChat)
	content := extractContent(parsed)
	modelCode := routed.Result.Model
	return TestResult{
		OK: true, Model: &modelCode, Content: content, LatencyMs: routed.Result.LatencyMs,
		Usage: usagePtr, Attempts: routed.Result.Attempts,
	}
}

func extractContent(data any) *string {
	record, ok := data.(map[string]any)
	if !ok {
		return nil
	}
	choices, _ := record["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return nil
	}
	if message, ok := first["message"].(map[string]any); ok {
		if content, ok := message["content"].(string); ok {
			return &content
		}
	}
	if text, ok := first["text"].(string); ok {
		return &text
	}
	return nil
}

// UpdateChainModels normalizes, persists and applies a new chain order.
func (r *Runtime) UpdateChainModels(models []string) error {
	next := make([]string, 0, len(models))
	seen := map[string]bool{}
	r.configMu.RLock()
	cfg := r.config
	r.configMu.RUnlock()
	for _, raw := range models {
		parsed, ok := core.SplitModelCode(strings.TrimSpace(raw), cfg.Providers)
		if !ok {
			return &ConfigRuleError{Message: fmt.Sprintf("模型 %s 没有指定提供商，请写成「提供商/模型名」", raw)}
		}
		code := core.JoinModelCode(parsed.Provider, parsed.Upstream)
		if !seen[code] {
			seen[code] = true
			next = append(next, code)
		}
	}
	previous := cfg.Chain.Models
	added := difference(next, previous)
	removed := difference(previous, next)
	cfg.Chain.Models = next
	r.Store.Ensure(next)
	r.Store.PruneModels(next)
	if err := config.SaveConfig(r.configPath, cfg); err != nil {
		return err
	}
	r.setConfig(cfg)
	message := "模型列表顺序已更新"
	if len(added) > 0 && len(removed) == 0 {
		message = "模型列表已加入 " + strings.Join(added, ", ")
	} else if len(removed) > 0 && len(added) == 0 {
		message = "模型列表已移除 " + strings.Join(removed, ", ")
	} else if len(added) > 0 || len(removed) > 0 {
		message = fmt.Sprintf("模型列表已更新（加入 %d 个，移除 %d 个）", len(added), len(removed))
	}
	r.Events.Push(stream.EventInput{Kind: stream.EventConfig, Level: stream.LevelInfo, Message: message})
	return nil
}

// UpdateChainContext validates and persists a context patch.
func (r *Runtime) UpdateChainContext(window *int, onOverflow *model.ContextOverflowMode) (model.ContextConfig, error) {
	r.configMu.RLock()
	cfg := r.config
	r.configMu.RUnlock()
	nextWindow := cfg.Chain.Context.Window
	if window != nil {
		nextWindow = *window
	}
	if nextWindow < 1000 || nextWindow > 10_000_000 {
		return model.ContextConfig{}, &ConfigRuleError{Message: "context.window 必须是 1000 到 10000000 之间的数字"}
	}
	nextOverflow := cfg.Chain.Context.OnOverflow
	if onOverflow != nil {
		if *onOverflow != model.OverflowReject && *onOverflow != model.OverflowPassthrough {
			return model.ContextConfig{}, &ConfigRuleError{Message: "context.onOverflow 只能是 reject / passthrough"}
		}
		nextOverflow = *onOverflow
	}
	cfg.Chain.Context = model.ContextConfig{Window: nextWindow, OnOverflow: nextOverflow}
	if err := config.SaveConfig(r.configPath, cfg); err != nil {
		return model.ContextConfig{}, err
	}
	r.setConfig(cfg)
	r.Events.Push(stream.EventInput{
		Kind: stream.EventConfig, Level: stream.LevelInfo,
		Message: fmt.Sprintf("上下文窗口已更新为 %d", nextWindow),
	})
	return cfg.Chain.Context, nil
}

// UpdateModelApis persists one model's protocol capabilities.
func (r *Runtime) UpdateModelApis(code string, apis []model.ApiKind) error {
	r.configMu.RLock()
	cfg := r.config
	r.configMu.RUnlock()
	parsed, ok := core.SplitModelCode(strings.TrimSpace(code), cfg.Providers)
	if !ok {
		return &ConfigRuleError{Message: fmt.Sprintf("模型 %s 没有指定提供商，请写成「提供商/模型名」", code)}
	}
	canonical := core.JoinModelCode(parsed.Provider, parsed.Upstream)
	if cfg.ModelApis == nil {
		cfg.ModelApis = map[string][]model.ApiKind{}
	}
	cfg.ModelApis[canonical] = append([]model.ApiKind(nil), apis...)
	if err := config.SaveConfig(r.configPath, cfg); err != nil {
		return err
	}
	r.setConfig(cfg)
	r.Events.Push(stream.EventInput{
		Kind: stream.EventConfig, Level: stream.LevelInfo,
		Message: fmt.Sprintf("%s 的 API 能力已更新为 %s", canonical, joinKinds(apis)),
		Model:   &canonical, Detail: map[string]any{"apis": apis},
	})
	return nil
}

func joinKinds(apis []model.ApiKind) string {
	parts := make([]string, len(apis))
	for index, api := range apis {
		parts[index] = string(api)
	}
	return strings.Join(parts, " / ")
}

// ApplyConfig swaps in a normalized configuration.
func (r *Runtime) ApplyConfig(next model.GatewayConfig, persist bool) ([]string, error) {
	r.configMu.Lock()
	previous := r.config
	r.configMu.Unlock()
	if _, overridden := os.LookupEnv("LITEROUTER_LOG_LEVEL"); !overridden {
		r.Logger.SetLevel(next.Log.Level)
	}
	r.Logger.SetRotation(next.Log.MaxBytes, next.Log.MaxFiles)
	if persist {
		if err := config.SaveConfig(r.configPath, next); err != nil {
			return nil, err
		}
	}
	r.setConfig(next)
	r.Store.Ensure(next.Chain.Models)
	r.Store.PruneModels(next.Chain.Models)
	message := "配置已保存"
	if !persist {
		message = "配置已从文件重新加载"
	}
	r.Events.Push(stream.EventInput{Kind: stream.EventConfig, Level: stream.LevelInfo, Message: message})
	var warnings []string
	if previous.Port != next.Port {
		warnings = append(warnings, "端口变更需要重启进程后生效")
	}
	if previous.Host != next.Host {
		warnings = append(warnings, "监听地址变更需要重启进程后生效")
	}
	return warnings, nil
}

func (r *Runtime) setConfig(cfg model.GatewayConfig) {
	r.configMu.Lock()
	r.config = cfg
	r.configMu.Unlock()
}

// InitializeConfigMessage pushes the startup event with the given chain count message.
func (r *Runtime) PushConfigEvent(message string) {
	r.Events.Push(stream.EventInput{Kind: stream.EventConfig, Level: stream.LevelInfo, Message: message})
}

// ResetUsage clears counters.
func (r *Runtime) ResetUsage() {
	r.Store.ResetUsage()
	r.Events.Push(stream.EventInput{Kind: stream.EventConfig, Level: stream.LevelInfo, Message: "用量统计已重置"})
}

// BackfillCosts recomputes historical costs using one instant's tier.
func (r *Runtime) BackfillCosts(at time.Time) {
	cfg := r.ConfigSnapshot()
	r.Store.RecomputeCosts(func(modelCode string, usage store.UsageTokens) model.CostSplit {
		if modelCode == "" {
			return model.CostSplit{}
		}
		split, ok := core.CostForUsage(cfg.Pricing, modelCode, modelTokenUsage(usage), at)
		if !ok {
			return model.CostSplit{}
		}
		return split
	})
	r.Events.Push(stream.EventInput{
		Kind: stream.EventConfig, Level: stream.LevelInfo,
		Message: fmt.Sprintf("成本已按 %s 的单价重算", at.Format("2006/01/02 15:04:05")),
	})
}

// Revive resets a model to unknown.
func (r *Runtime) Revive(code string) {
	r.Store.SetHealth(code, model.HealthUnknown, map[string]any{
		"reason": nil, "lastErrorCode": nil, "lastStatusCode": nil,
		"lastErrorMessage": nil, "throttledUntil": nil, "checkedAt": nil,
	})
	r.Events.Push(stream.EventInput{Kind: stream.EventRevive, Level: stream.LevelInfo, Message: code + " 已重置为待检测", Model: &code})
}

// Bootstrap emits the startup event and key warnings.
func (r *Runtime) Bootstrap() {
	cfg := r.ConfigSnapshot()
	chain := r.ChainCodes()
	primary := "未选择"
	if len(chain) > 0 {
		primary = chain[0]
	}
	providerIDs := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		providerIDs = append(providerIDs, p.ID)
	}
	r.Events.Push(stream.EventInput{
		Kind: stream.EventStart, Level: stream.LevelInfo,
		Message: fmt.Sprintf("literouter 已启动，%d 个模型 / %d 个提供商 · 当前 %s", len(chain), len(cfg.Providers), primary),
		Detail:  map[string]any{"providers": providerIDs},
	})
	infos := r.ProviderKeyInfos()
	ready := 0
	var missing []string
	for _, info := range infos {
		if info.Configured {
			ready++
		} else {
			missing = append(missing, info.ID)
		}
	}
	if ready == 0 {
		r.Logger.Info("所有提供商都未配置 API Key，/v1 请求会返回 503 literouter.no_key", map[string]any{
			"providers": strings.Join(missing, ", "),
			"hint":      ".env / config.json 的 providers[].apiKey",
		})
		return
	}
	if len(missing) > 0 {
		r.Logger.Warn("部分提供商没有 Key，路由到它们的请求会返回 503", map[string]any{"missing": strings.Join(missing, ", ")})
	}
}

// Shutdown flushes state and events.
func (r *Runtime) Shutdown() {
	_ = r.Store.Flush()
	_ = r.Events.Flush()
}

// SanitizedConfig removes API keys for display.
func (r *Runtime) SanitizedConfig() model.GatewayConfig {
	cfg := r.ConfigSnapshot()
	for index := range cfg.Providers {
		cfg.Providers[index].APIKey = ""
	}
	return cfg
}

// SanitizedConfigText renders the sanitized config as pretty JSON.
func (r *Runtime) SanitizedConfigText() string {
	cfg := r.SanitizedConfig()
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "{}\n"
	}
	return string(encoded) + "\n"
}

// SetConfigFromAdmin preserves inline keys by provider id.
func (r *Runtime) SetConfigFromAdmin(next model.GatewayConfig) ([]string, error) {
	r.configMu.RLock()
	previous := r.config
	r.configMu.RUnlock()
	for index := range next.Providers {
		if next.Providers[index].APIKey != "" {
			continue
		}
		for _, old := range previous.Providers {
			if old.ID == next.Providers[index].ID && old.APIKey != "" {
				next.Providers[index].APIKey = old.APIKey
				break
			}
		}
	}
	return r.ApplyConfig(next, true)
}

func isChatPath(path string) bool {
	return path == "chat/completions" || path == "completions" || path == "responses"
}

func collectCodes(cfg model.GatewayConfig, extra []string) []string {
	seen := map[string]bool{}
	codes := make([]string, 0, len(cfg.Chain.Models)+len(extra))
	for _, code := range append(append([]string(nil), extra...), cfg.Chain.Models...) {
		if code != "" && !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	return codes
}

func collectModelAliases(cfg model.GatewayConfig) map[string]string {
	byUpstream := map[string][]string{}
	for _, code := range cfg.Chain.Models {
		parsed, ok := core.SplitModelCode(code, cfg.Providers)
		if !ok {
			continue
		}
		byUpstream[parsed.Upstream] = append(byUpstream[parsed.Upstream], code)
	}
	aliases := map[string]string{}
	for upstream, codes := range byUpstream {
		if len(codes) == 1 {
			aliases[upstream] = codes[0]
		}
	}
	return aliases
}

func summarizeAttempts(attempts []model.AttemptRecord) []map[string]any {
	out := make([]map[string]any, 0, len(attempts))
	for _, attempt := range attempts {
		out = append(out, map[string]any{
			"model": attempt.Model, "ok": attempt.OK, "kind": attempt.Kind,
			"status": attempt.Status, "code": attempt.Code, "latencyMs": attempt.LatencyMs,
		})
	}
	return out
}

func difference(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, value := range b {
		set[value] = true
	}
	out := make([]string, 0)
	for _, value := range a {
		if !set[value] {
			out = append(out, value)
		}
	}
	return out
}

func isoTimestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// SortCallers orders caller stats the way the dashboard expects.
func SortCallers(callers []store.CallerRecord) {
	sort.SliceStable(callers, func(i, j int) bool {
		leftTokens := callers[i].PromptTokens + callers[i].CompletionTokens
		rightTokens := callers[j].PromptTokens + callers[j].CompletionTokens
		if leftTokens != rightTokens {
			return leftTokens > rightTokens
		}
		if callers[i].Requests != callers[j].Requests {
			return callers[i].Requests > callers[j].Requests
		}
		return callers[i].Label < callers[j].Label
	})
}
