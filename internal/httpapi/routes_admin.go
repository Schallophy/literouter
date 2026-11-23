package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Schallophy/literouter/internal/config"
	"github.com/Schallophy/literouter/internal/core"
	"github.com/Schallophy/literouter/internal/model"
	"github.com/Schallophy/literouter/internal/runtime"
	"github.com/Schallophy/literouter/internal/store"
	"github.com/Schallophy/literouter/internal/stream"
)

var eventKinds = map[stream.EventKind]bool{
	stream.EventStart: true, stream.EventFailover: true, stream.EventDead: true,
	stream.EventThrottle: true, stream.EventRevive: true, stream.EventCatalog: true,
	stream.EventConfig: true, stream.EventError: true,
}

func (s *Server) registerAdmin() {
	s.add("GET", "/health", s.handleHealth)
	s.add("GET", "/api/overview", s.handleOverview)
	s.add("GET", "/api/capability", s.handleCapability)
	s.add("GET", "/api/events", s.handleEvents)
	s.add("GET", "/api/live", s.handleLiveStream)
	s.add("DELETE", "/api/live", s.handleLiveClear)
	s.add("GET", "/api/live/:id/raw", s.handleLiveRaw)
	s.add("GET", "/api/config", s.handleConfigGet)
	s.add("PUT", "/api/config", s.handleConfigPut)
	s.add("PATCH", "/api/chain", s.handleChainContext)
	s.add("POST", "/api/usage/reset", s.handleUsageReset)
	s.add("POST", "/api/usage/backfill-cost", s.handleBackfillCost)
	s.add("POST", "/api/models/:code/revive", s.handleRevive)
	s.add("POST", "/api/models/:code/apis", s.handleModelApis)
	s.add("POST", "/api/chain/order", s.handleChainOrder)
	s.add("POST", "/api/chain/models", s.handleChainAdd)
	s.add("POST", "/api/chain/models/remove", s.handleChainRemove)
	s.add("DELETE", "/api/chain/models/:code", s.handleChainDelete)
	s.add("POST", "/api/catalog/refresh", s.handleCatalogRefresh)
	s.add("POST", "/api/test", s.handleTest)
}

func (s *Server) handleHealth(ctx *Ctx) error {
	rt := s.Runtime
	chain := rt.ChainSnapshot()
	keys := rt.ProviderKeyInfos()
	configured := 0
	for _, key := range keys {
		if key.Configured {
			configured++
		}
	}
	ctx.Send(200, map[string]any{
		"status":        "ok",
		"version":       Version,
		"uptimeSeconds": uptimeSeconds(rt),
		"bootedAt":      rt.BootedAt().UTC().Format("2006-01-02T15:04:05.000Z"),
		"model":         rt.PrimaryModel(),
		"models": map[string]any{
			"total": len(chain.Models), "alive": chain.Alive, "dead": chain.Dead,
		},
		"providers": map[string]any{"total": len(keys), "configured": configured},
	}, map[string]string{"cache-control": "no-store"})
	return nil
}

func (s *Server) handleOverview(ctx *Ctx) error {
	rt := s.Runtime
	cfg := rt.ConfigSnapshot()
	tracked := map[string]bool{}
	for _, code := range cfg.Chain.Models {
		tracked[code] = true
	}

	fetchedAt, entries := rt.Store.Catalog()
	catalog := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		record := rt.Store.Get(entry.Code)
		row := map[string]any{
			"code":     entry.Code,
			"upstream": entry.Upstream,
			"family":   entry.Family,
			"tracked":  tracked[entry.Code],
			"health":   nil,
			"lastStatusCode": nil,
			"lastErrorCode":  nil,
			"apis":     rt.ApisFor(entry.Code),
			"provider": entry.Provider,
		}
		if row["provider"] == "" {
			row["provider"] = rt.ProviderIDFor(entry.Code)
		}
		if record != nil {
			row["health"] = record.Health
			row["lastStatusCode"] = record.LastStatusCode
			row["lastErrorCode"] = record.LastErrorCode
		}
		catalog = append(catalog, row)
	}

	var fetched any
	if fetchedAt != "" {
		fetched = fetchedAt
	}

	callers := rt.Store.Callers()
	runtime.SortCallers(callers)
	callerViews := make([]callerView, 0, len(callers))
	for _, caller := range callers {
		copied := caller
		models := make([]*store.CallerModelStat, 0, len(copied.Models))
		for _, stat := range copied.Models {
			models = append(models, stat)
		}
		sort.SliceStable(models, func(i, j int) bool {
			if models[i].Cost != models[j].Cost {
				return models[i].Cost > models[j].Cost
			}
			if models[i].Requests != models[j].Requests {
				return models[i].Requests > models[j].Requests
			}
			return modelName(models[i]) < modelName(models[j])
		})
		callerViews = append(callerViews, callerView{CallerRecord: copied, Models: models})
	}

	providers := make([]map[string]any, 0, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		providers = append(providers, map[string]any{
			"id": provider.ID, "label": provider.Label, "baseUrl": provider.BaseURL,
			"apiKeyEnv": provider.APIKeyEnv, "requestTimeoutMs": provider.RequestTimeoutMs,
			"injectStreamUsage": provider.InjectStreamUsage,
		})
	}

	host := cfg.Host
	endpointHost := host
	if host == "0.0.0.0" {
		endpointHost = "127.0.0.1"
	}

	ctx.Send(200, map[string]any{
		"app": map[string]any{
			"name":          "literouter",
			"version":       Version,
			"now":           isoTimestamp(),
			"bootedAt":      rt.BootedAt().UTC().Format("2006-01-02T15:04:05.000Z"),
			"uptimeSeconds": uptimeSeconds(rt),
			"host":          host,
			"port":          cfg.Port,
			"endpoint":      fmt.Sprintf("http://%s:%d/v1", endpointHost, cfg.Port),
			"configPath":    rt.ConfigPath(),
			"dataDir":       rt.DataDir(),
		},
		"key":             rt.KeyInfo(),
		"providers":       providers,
		"defaultProvider": cfg.DefaultProvider,
		"pricing":         map[string]any{"currency": cfg.Pricing.Currency, "unit": cfg.Pricing.Unit, "preferOffPeak": cfg.Pricing.PreferOffPeak},
		"keys":            rt.ProviderKeyInfos(),
		"ui":              cfg.UI,
		"server":          cfg.Server,
		"log":             cfg.Log,
		"stats":           rt.Store.Stats(),
		"chaining":        rt.ChainStats(),
		"callers":         callerViews,
		"chain":           rt.ChainSnapshot(),
		"output":          rt.LastOutput(),
		"activeModel":     rt.PrimaryModel(),
		"catalog":         map[string]any{"fetchedAt": fetched, "models": catalog},
		"events":          rt.Events.Recent(120, ""),
		"warnings":        rt.Warnings(),
	}, nil)
	return nil
}

type callerView struct {
	store.CallerRecord
	Models []*store.CallerModelStat `json:"models"`
}

func modelName(stat *store.CallerModelStat) string {
	if stat.Model == nil {
		return ""
	}
	return *stat.Model
}

func (s *Server) handleCapability(ctx *Ctx) error {
	rt := s.Runtime
	refresh := ctx.URL.Query().Get("refresh") == "1"
	snapshot := rt.Capability().Snapshot(refresh, time.Now())
	tracked := map[string]bool{}
	for _, code := range rt.ChainCodes() {
		tracked[code] = true
	}
	_, entries := rt.Store.Catalog()
	models := make([]map[string]any, 0, len(entries))
	matchedCount := 0
	for _, entry := range entries {
		match, matched := core.MatchCapability(snapshot.Index, entry.Upstream)
		record := rt.Store.Get(entry.Code)
		provider := entry.Provider
		if provider == "" {
			provider = rt.ProviderIDFor(entry.Code)
		}
		var health any
		if record != nil {
			health = record.Health
		}
		row := map[string]any{
			"code": entry.Code, "upstream": entry.Upstream, "family": entry.Family,
			"provider": provider, "tracked": tracked[entry.Code], "health": health,
			"score": nil, "matchedSlug": nil, "matchedName": nil,
			"reasoning": nil, "contextWindow": nil,
		}
		if matched {
			row["score"] = match.Model.Score
			row["matchedSlug"] = match.Model.Slug
			row["matchedName"] = match.Model.Name
			reasoning := match.Model.Reasoning
			row["reasoning"] = &reasoning
			row["contextWindow"] = match.Model.ContextWindow
			if match.Model.Score != nil {
				matchedCount++
			}
		}
		models = append(models, row)
	}
	sort.SliceStable(models, func(i, j int) bool {
		left, right := scoreOf(models[i]), scoreOf(models[j])
		if left != right {
			return left > right
		}
		return models[i]["code"].(string) < models[j]["code"].(string)
	})

	var errorValue any
	if snapshot.Error != nil {
		errorValue = *snapshot.Error
	}
	var fetchedAt any
	if snapshot.FetchedAt != nil {
		fetchedAt = *snapshot.FetchedAt
	}
	ctx.Send(200, map[string]any{
		"ok":    snapshot.Error == nil,
		"error": errorValue,
		"source": map[string]any{
			"name": "Artificial Analysis", "url": core.LeaderboardURL,
			"fetchedAt": fetchedAt, "cached": snapshot.Cached,
			"total": snapshot.Total, "scored": snapshot.Scored, "matched": matchedCount,
		},
		"models": models,
	}, map[string]string{"cache-control": "no-store"})
	return nil
}

func scoreOf(row map[string]any) float64 {
	if score, ok := row["score"].(*float64); ok && score != nil {
		return *score
	}
	return math.Inf(-1)
}

func (s *Server) handleEvents(ctx *Ctx) error {
	limit := 200
	if raw := ctx.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			limit = int(math.Trunc(parsed))
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 400 {
		limit = 400
	}
	kind := stream.EventKind(ctx.URL.Query().Get("kind"))
	if !eventKinds[kind] {
		kind = ""
	}
	ctx.Send(200, map[string]any{"events": s.Runtime.Events.Recent(limit, kind)}, nil)
	return nil
}

func (s *Server) handleLiveStream(ctx *Ctx) error {
	header := ctx.W.Header()
	header.Set("content-type", "text/event-stream; charset=utf-8")
	header.Set("cache-control", "no-cache, no-transform")
	header.Set("connection", "keep-alive")
	header.Set("x-accel-buffering", "no")
	ctx.W.WriteHeader(200)
	ctx.W.Flush()

	var mutex sync.Mutex
	send := func(event string, data any) {
		mutex.Lock()
		defer mutex.Unlock()
		encoded, err := json.Marshal(data)
		if err != nil {
			return
		}
		if _, writeErr := fmt.Fprintf(ctx.W, "event: %s\ndata: %s\n\n", event, encoded); writeErr != nil {
			return
		}
		ctx.W.Flush()
	}

	send("snapshot", map[string]any{
		"sessions": s.Runtime.LiveOutput.Recent(0),
		"limits":   s.Runtime.LiveOutput.Limits(),
	})
	unsubscribe := s.Runtime.LiveOutput.Subscribe(func(event stream.LiveOutputEvent) {
		send(event.Type, event)
	})
	defer unsubscribe()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.R.Context().Done():
			return nil
		case <-ticker.C:
			mutex.Lock()
			_, err := fmt.Fprint(ctx.W, ": ping\n\n")
			if err == nil {
				ctx.W.Flush()
			}
			mutex.Unlock()
			if err != nil {
				return nil
			}
		}
	}
}

func (s *Server) handleLiveClear(ctx *Ctx) error {
	s.Runtime.LiveOutput.Clear()
	ctx.Send(200, map[string]any{"ok": true}, nil)
	return nil
}

func (s *Server) handleLiveRaw(ctx *Ctx) error {
	id, err := strconv.ParseInt(ctx.Params["id"], 10, 64)
	if err != nil || id <= 0 {
		return &HttpError{Status: 400, Message: "无效的输出编号"}
	}
	raw, truncated, ok := s.Runtime.LiveOutput.Raw(id)
	if !ok {
		return &HttpError{Status: 404, Message: "找不到该输出"}
	}
	ctx.Send(200, map[string]any{"raw": raw, "truncated": truncated}, nil)
	return nil
}

func (s *Server) handleConfigGet(ctx *Ctx) error {
	rt := s.Runtime
	ctx.Send(200, map[string]any{
		"text":       rt.SanitizedConfigText(),
		"config":     rt.SanitizedConfig(),
		"apiKeySet":  rt.KeyInfo().Configured,
		"keys":       rt.ProviderKeyInfos(),
		"key":        rt.KeyInfo(),
		"configPath": rt.ConfigPath(),
		"dataDir":    rt.DataDir(),
	}, nil)
	return nil
}

func (s *Server) handleConfigPut(ctx *Ctx) error {
	rt := s.Runtime
	body, ok := ctx.Body.(map[string]any)
	if !ok {
		return &HttpError{Status: 400, Message: "请求体必须是 JSON 对象"}
	}
	var parsed any
	if text, ok := body["text"].(string); ok {
		value, err := config.ParseConfigText(text)
		if err != nil {
			ctx.Send(400, map[string]any{
				"error":  map[string]any{"message": err.Error(), "type": "literouter.config", "code": "literouter.config"},
				"errors": []string{err.Error()},
			}, nil)
			return nil
		}
		parsed = value
	} else if raw, exists := body["config"]; exists {
		patch, ok := raw.(map[string]any)
		if !ok {
			return &HttpError{Status: 400, Message: "config 必须是 JSON 对象"}
		}
		base := toJSONMap(rt.SanitizedConfig())
		parsed = deepMerge(base, patch)
	} else {
		return &HttpError{Status: 400, Message: "需要提供 config（对象）或 text（字符串）"}
	}

	normalized := config.Normalize(parsed)
	if !normalized.OK {
		ctx.Send(400, map[string]any{
			"error":  map[string]any{"message": "配置校验失败", "type": "literouter.config", "code": "literouter.config"},
			"errors": normalized.Errors,
		}, nil)
		return nil
	}

	warnings, err := rt.SetConfigFromAdmin(normalized.Config)
	if err != nil {
		return err
	}
	combined := append(append([]string(nil), normalized.Warnings...), warnings...)
	ctx.Send(200, map[string]any{
		"ok": true, "warnings": combined, "config": rt.SanitizedConfig(),
	}, nil)
	return nil
}

func (s *Server) handleChainContext(ctx *Ctx) error {
	body, ok := ctx.Body.(map[string]any)
	if !ok {
		return &HttpError{Status: 400, Message: "请求体必须是 JSON 对象"}
	}
	contextPatch, ok := body["context"].(map[string]any)
	if !ok {
		return &HttpError{Status: 400, Message: "context 必须是对象"}
	}
	var window *int
	if raw, exists := contextPatch["window"]; exists {
		value := jsNumber(raw)
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(math.MaxInt32) {
			value = 0
		}
		rounded := int(math.Round(value))
		window = &rounded
	}
	var overflow *model.ContextOverflowMode
	if raw, ok := contextPatch["onOverflow"].(string); ok {
		mode := model.ContextOverflowMode(raw)
		overflow = &mode
	}
	next, err := s.Runtime.UpdateChainContext(window, overflow)
	if err != nil {
		return s.guardConfigError(ctx, err)
	}
	ctx.Send(200, map[string]any{"ok": true, "context": next}, nil)
	return nil
}

func (s *Server) guardConfigError(ctx *Ctx, err error) error {
	var rule *runtime.ConfigRuleError
	if errors.As(err, &rule) {
		payload := map[string]any{
			"error": map[string]any{"message": rule.Message, "type": "literouter.config", "code": "literouter.config"},
		}
		if len(rule.Details) > 0 {
			payload["errors"] = rule.Details
		}
		ctx.Send(400, payload, nil)
		return nil
	}
	return err
}

func (s *Server) handleUsageReset(ctx *Ctx) error {
	s.Runtime.ResetUsage()
	ctx.Send(200, map[string]any{"ok": true, "stats": s.Runtime.Store.Stats()}, nil)
	return nil
}

func (s *Server) handleBackfillCost(ctx *Ctx) error {
	body, _ := ctx.Body.(map[string]any)
	var at time.Time
	if raw, ok := body["at"].(string); ok {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return &HttpError{Status: 400, Message: "at 必须是合法的 ISO 时间"}
		}
		at = parsed
	} else {
		at = time.Now()
	}
	s.Runtime.BackfillCosts(at)
	ctx.Send(200, map[string]any{
		"ok": true, "at": at.UTC().Format("2006-01-02T15:04:05.000Z"), "stats": s.Runtime.Store.Stats(),
	}, nil)
	return nil
}

func (s *Server) handleRevive(ctx *Ctx) error {
	code := ctx.Params["code"]
	if s.Runtime.Store.Get(code) == nil {
		ctx.Send(404, map[string]any{"error": map[string]any{
			"message": "未知模型：" + code, "type": "literouter.not_found", "code": "literouter.not_found",
		}}, nil)
		return nil
	}
	s.Runtime.Revive(code)
	ctx.Send(200, map[string]any{"ok": true, "model": s.Runtime.SnapshotModel(code)}, nil)
	return nil
}

func (s *Server) handleModelApis(ctx *Ctx) error {
	code := strings.TrimSpace(ctx.Params["code"])
	if code == "" {
		return &HttpError{Status: 400, Message: "缺少模型 code"}
	}
	body, ok := ctx.Body.(map[string]any)
	if !ok {
		return &HttpError{Status: 400, Message: "请求体必须是 JSON 对象"}
	}
	rawList, ok := body["apis"].([]any)
	if !ok {
		return &HttpError{Status: 400, Message: "apis 必须是数组"}
	}
	apis := []model.ApiKind{}
	seen := map[model.ApiKind]bool{}
	for _, entry := range rawList {
		text, ok := entry.(string)
		if !ok || !model.IsApiKind(text) {
			return &HttpError{Status: 400, Message: fmt.Sprintf("未知 API：%v", entry)}
		}
		kind := model.ApiKind(text)
		if !seen[kind] {
			seen[kind] = true
			apis = append(apis, kind)
		}
	}
	if len(apis) == 0 {
		return &HttpError{Status: 400, Message: "至少需要一个 API"}
	}
	if err := s.Runtime.UpdateModelApis(code, apis); err != nil {
		return s.guardConfigError(ctx, err)
	}
	ctx.Send(200, map[string]any{"ok": true, "code": code, "apis": s.Runtime.ApisFor(code)}, nil)
	return nil
}

func (s *Server) handleChainOrder(ctx *Ctx) error {
	body, ok := ctx.Body.(map[string]any)
	if !ok {
		return &HttpError{Status: 400, Message: "请求体必须是 JSON 对象"}
	}
	list, _ := body["models"].([]any)
	models := uniqueStrings(list)
	if len(models) == 0 {
		return &HttpError{Status: 400, Message: "models 必须是非空数组"}
	}
	if err := s.Runtime.UpdateChainModels(models); err != nil {
		return s.guardConfigError(ctx, err)
	}
	ctx.Send(200, map[string]any{"ok": true, "chain": map[string]any{"models": s.Runtime.ChainCodes()}}, nil)
	return nil
}

func (s *Server) handleChainAdd(ctx *Ctx) error {
	body, ok := ctx.Body.(map[string]any)
	if !ok {
		return &HttpError{Status: 400, Message: "请求体必须是 JSON 对象"}
	}
	code := ""
	if value, ok := body["code"].(string); ok {
		code = strings.TrimSpace(value)
	}
	if code == "" {
		return &HttpError{Status: 400, Message: "code 不能为空"}
	}
	models := s.Runtime.ChainCodes()
	found := false
	for _, existing := range models {
		if existing == code {
			found = true
			break
		}
	}
	if !found {
		models = append(models, code)
	}
	if err := s.Runtime.UpdateChainModels(models); err != nil {
		return s.guardConfigError(ctx, err)
	}
	ctx.Send(200, map[string]any{"ok": true, "chain": map[string]any{"models": s.Runtime.ChainCodes()}}, nil)
	return nil
}

func (s *Server) handleChainRemove(ctx *Ctx) error {
	body, ok := ctx.Body.(map[string]any)
	if !ok {
		return &HttpError{Status: 400, Message: "请求体必须是 JSON 对象"}
	}
	list, _ := body["models"].([]any)
	remove := map[string]bool{}
	for _, entry := range list {
		if text, ok := entry.(string); ok {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				remove[trimmed] = true
			}
		}
	}
	if len(remove) == 0 {
		return &HttpError{Status: 400, Message: "models 必须是非空数组"}
	}
	current := s.Runtime.ChainCodes()
	next := make([]string, 0, len(current))
	for _, code := range current {
		if !remove[code] {
			next = append(next, code)
		}
	}
	if len(next) == len(current) {
		ctx.Send(200, map[string]any{"ok": true, "removed": 0, "chain": map[string]any{"models": current}}, nil)
		return nil
	}
	if err := s.Runtime.UpdateChainModels(next); err != nil {
		return s.guardConfigError(ctx, err)
	}
	ctx.Send(200, map[string]any{
		"ok": true, "removed": len(current) - len(next), "chain": map[string]any{"models": next},
	}, nil)
	return nil
}

func (s *Server) handleChainDelete(ctx *Ctx) error {
	code := ctx.Params["code"]
	current := s.Runtime.ChainCodes()
	next := make([]string, 0, len(current))
	for _, item := range current {
		if item != code {
			next = append(next, item)
		}
	}
	if len(next) == len(current) {
		ctx.Send(404, map[string]any{"error": map[string]any{
			"message": "模型列表中没有 " + code, "type": "literouter.not_found", "code": "literouter.not_found",
		}}, nil)
		return nil
	}
	if err := s.Runtime.UpdateChainModels(next); err != nil {
		return s.guardConfigError(ctx, err)
	}
	ctx.Send(200, map[string]any{"ok": true, "chain": map[string]any{"models": next}}, nil)
	return nil
}

func (s *Server) handleCatalogRefresh(ctx *Ctx) error {
	body, _ := ctx.Body.(map[string]any)
	provider := ""
	if value, ok := body["provider"].(string); ok {
		provider = strings.TrimSpace(value)
	}
	ok, count, message := s.Runtime.RefreshCatalog(provider)
	if !ok {
		if message == "" {
			message = "模型目录刷新失败"
		}
		ctx.Send(502, map[string]any{"error": map[string]any{
			"message": message, "type": "literouter.catalog", "code": "literouter.catalog",
		}}, nil)
		return nil
	}
	warnings := []string{}
	if message != "" {
		warnings = append(warnings, message)
	}
	ctx.Send(200, map[string]any{"ok": true, "count": count, "warnings": warnings}, nil)
	return nil
}

func (s *Server) handleTest(ctx *Ctx) error {
	body, _ := ctx.Body.(map[string]any)
	prompt := "你好，请回复一句话。"
	if value, ok := body["prompt"].(string); ok && strings.TrimSpace(value) != "" {
		prompt = strings.TrimSpace(value)
	}
	ctx.Send(200, s.Runtime.Test(prompt), nil)
	return nil
}

func uptimeSeconds(rt *runtime.Runtime) int64 {
	return int64(math.Round(time.Since(rt.BootedAt()).Seconds()))
}

func isoTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func uniqueStrings(values []any) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

func jsNumber(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return math.NaN()
		}
		return parsed
	case bool:
		if typed {
			return 1
		}
		return 0
	case nil:
		return 0
	default:
		return math.NaN()
	}
}

func toJSONMap(value any) map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func deepMerge(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range patch {
		patchMap, patchIsMap := value.(map[string]any)
		baseMap, baseIsMap := out[key].(map[string]any)
		if patchIsMap && baseIsMap {
			out[key] = deepMerge(baseMap, patchMap)
			continue
		}
		out[key] = value
	}
	return out
}
