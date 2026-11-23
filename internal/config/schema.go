package config

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Schallophy/literouter/internal/core"
	"github.com/Schallophy/literouter/internal/model"
)

const (
	DefaultContextWindow       = 256000
	DefaultRetryDeadMinutes    = 1440
	DefaultLogMaxBytes   int64 = 2 * 1024 * 1024
	DefaultLogMaxFiles         = 3
	DefaultMaxBodyBytes        = 16 * 1024 * 1024
	DefaultBaseURL             = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	DefaultPricingCurrency     = "CNY"
	DefaultPricingUnit   int64 = 1_000_000
)

var ProviderIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

func IsValidProviderID(value string) bool { return ProviderIDPattern.MatchString(value) }

// DefaultProviders returns a fresh copy of the shipped provider list.
func DefaultProviders() []model.ProviderConfig {
	return []model.ProviderConfig{{
		ID:                "dashscope",
		Label:             "阿里云百炼",
		BaseURL:           DefaultBaseURL,
		APIKeyEnv:         "DASHSCOPE_API_KEY",
		APIKey:            "",
		RequestTimeoutMs:  90000,
		InjectStreamUsage: true,
	}}
}

// DefaultConfig mirrors defaultConfig() in the TypeScript implementation.
func DefaultConfig() model.GatewayConfig {
	providers := DefaultProviders()
	return model.GatewayConfig{
		Version:      1,
		Host:         "127.0.0.1",
		Port:         8788,
		GatewayToken: "",
		Server:       model.ServerConfig{MaxBodyBytes: DefaultMaxBodyBytes, Cors: true},
		UI:           model.UiConfig{Enabled: true},
		Log:          model.LogConfig{Level: model.LogInfo, File: "", MaxBytes: DefaultLogMaxBytes, MaxFiles: DefaultLogMaxFiles},
		Chain: model.ChainConfig{
			Models:                []string{},
			Context:               model.ContextConfig{Window: DefaultContextWindow, OnOverflow: model.OverflowReject},
			RetryDeadAfterMinutes: DefaultRetryDeadMinutes,
		},
		ModelApis:       map[string][]model.ApiKind{},
		Providers:       providers,
		DefaultProvider: providers[0].ID,
		Failover:        model.FailoverConfig{MaxAttempts: 8, ThrottleCooldownMs: 60000},
		Pricing:         model.PricingConfig{Currency: DefaultPricingCurrency, Unit: DefaultPricingUnit, PreferOffPeak: true, Models: map[string]model.ModelPricing{}},
		ResponsesChaining: model.ResponsesChainingConfig{
			Enabled: false, TTLMinutes: 120, MaxSessions: 500,
		},
	}
}

// NormalizeResult is the outcome of normalizing a config document.
type NormalizeResult struct {
	OK         bool
	Config     model.GatewayConfig
	Errors     []string
	Warnings   []string
	Migrations []string
}

// SystemTimeZone returns the IANA zone of the current process, or UTC.
func SystemTimeZone() string {
	name := time.Local.String()
	if name != "" && name != "Local" && name != "UTC" {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	return "UTC"
}

func IsTimeZone(value string) bool {
	_, err := time.LoadLocation(value)
	return err == nil
}

type object map[string]any

func isObject(value any) (object, bool) {
	obj, ok := value.(map[string]any)
	return object(obj), ok
}

func isArray(value any) bool {
	_, ok := value.([]any)
	return ok
}

func readString(source object, key, fallback string) string {
	if value, ok := source[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}

func readBool(source object, key string, fallback bool) bool {
	if value, ok := source[key].(bool); ok {
		return value
	}
	return fallback
}

func numberValue(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, !math.IsNaN(value) && !math.IsInf(value, 0)
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case interface{ Float64() (float64, error) }:
		f, err := value.Float64()
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return 0, true // JS Number("") === 0
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

func readNumber(source object, key string, fallback float64, min, max float64, path string, errors *[]string) float64 {
	raw, present := source[key]
	if !present {
		return fallback
	}
	// json.Number arrives as a distinct type; normalize it first.
	if n, ok := raw.(interface{ Float64() (float64, error) }); ok {
		if f, err := n.Float64(); err == nil {
			raw = f
		}
	}
	value, ok := numberValue(raw)
	if !ok || value < min || value > max {
		*errors = append(*errors, fmt.Sprintf("%s 必须是 %s 到 %s 之间的数字", path, formatNumber(min), formatNumber(max)))
		return fallback
	}
	return value
}

func formatNumber(value float64) string {
	if value == math.Trunc(value) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func readBaseURL(source object, fallback, path string, errors *[]string) string {
	raw, _ := source["baseUrl"].(string)
	value := fallback
	if strings.TrimSpace(raw) != "" {
		value = strings.TrimSpace(raw)
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		*errors = append(*errors, fmt.Sprintf("%s.baseUrl 必须是合法的 http(s) 地址", path))
		return fallback
	}
	return strings.TrimRight(value, "/")
}

func readProviderItem(item object, id, path string, errors *[]string) model.ProviderConfig {
	label := readString(item, "label", "")
	if label == "" {
		label = id
	}
	return model.ProviderConfig{
		ID:                id,
		Label:             label,
		BaseURL:           readBaseURL(item, DefaultBaseURL, path, errors),
		APIKeyEnv:         readString(item, "apiKeyEnv", ""),
		APIKey:            readString(item, "apiKey", ""),
		RequestTimeoutMs:  int(readNumber(item, "requestTimeoutMs", 90000, 1000, 600000, path+".requestTimeoutMs", errors)),
		InjectStreamUsage: readBool(item, "injectStreamUsage", true),
	}
}

func readProviders(source object, errors, warnings, migrations *[]string) ([]model.ProviderConfig, string) {
	var providers []model.ProviderConfig
	raw, present := source["providers"]
	if !present {
		if list, ok := source["upstreams"]; ok {
			if _, isList := list.([]any); isList {
				raw = list
				present = true
				*migrations = append(*migrations, "已把旧字段 upstreams 迁移为 providers")
			}
		}
	}

	if present && !isArray(raw) {
		*errors = append(*errors, "providers 必须是数组")
	}

	if list, ok := raw.([]any); ok && len(list) > 0 {
		seen := map[string]bool{}
		for index, entry := range list {
			path := fmt.Sprintf("providers[%d]", index)
			item, ok := isObject(entry)
			if !ok {
				*errors = append(*errors, path+" 必须是对象")
				continue
			}
			id := strings.ToLower(readString(item, "id", ""))
			if !IsValidProviderID(id) {
				*errors = append(*errors, path+".id 只能使用小写字母、数字和 . _ -，且不能为空")
				continue
			}
			if seen[id] {
				*errors = append(*errors, path+".id 重复: "+id)
				continue
			}
			seen[id] = true
			providers = append(providers, readProviderItem(item, id, path, errors))
		}
	} else if upstream, ok := isObject(source["upstream"]); ok {
		*migrations = append(*migrations, "已把旧版 upstream 单对象迁移为 providers[0]（id: default）")
		providers = append(providers, readProviderItem(upstream, "default", "upstream", errors))
	} else if list, ok := raw.([]any); ok && len(list) == 0 {
		*errors = append(*errors, "providers 至少需要一个提供商")
	}

	if len(providers) == 0 {
		if len(*errors) == 0 {
			*errors = append(*errors, "至少需要一个提供商")
		}
		defaults := DefaultProviders()
		return defaults, defaults[0].ID
	}

	requested := readString(source, "defaultProvider", "")
	if requested == "" {
		requested = readString(source, "defaultUpstream", "")
	}
	requested = strings.ToLower(requested)
	defaultProvider := requested
	if defaultProvider == "" {
		defaultProvider = providers[0].ID
	}
	found := false
	for _, provider := range providers {
		if provider.ID == defaultProvider {
			found = true
			break
		}
	}
	if !found {
		*warnings = append(*warnings, fmt.Sprintf("defaultProvider 指向不存在的提供商，已改为 %s", providers[0].ID))
		defaultProvider = providers[0].ID
	}
	return providers, defaultProvider
}

func readLegacyBindings(source object, providers []model.ProviderConfig, errors, migrations *[]string) map[string]string {
	raw, present := source["modelProviders"]
	if !present {
		if value, ok := source["modelUpstreams"]; ok {
			raw = value
			present = true
			*migrations = append(*migrations, "已把旧字段 modelUpstreams 迁移为 modelProviders")
		}
	}
	if !present || raw == nil {
		return map[string]string{}
	}
	obj, ok := isObject(raw)
	if !ok {
		*migrations = append(*migrations, "已忽略非对象的 modelProviders，改为在模型 code 中写提供商")
		return map[string]string{}
	}
	bindings := map[string]string{}
	for rawCode, rawValue := range obj {
		code := strings.TrimSpace(rawCode)
		value, isString := rawValue.(string)
		if code == "" || !isString || strings.TrimSpace(value) == "" {
			key := code
			if key == "" {
				key = "(empty)"
			}
			*errors = append(*errors, fmt.Sprintf("modelProviders.%s 必须是提供商 id", key))
			continue
		}
		id := strings.ToLower(strings.TrimSpace(value))
		known := false
		for _, provider := range providers {
			if provider.ID == id {
				known = true
				break
			}
		}
		if !known {
			*errors = append(*errors, fmt.Sprintf("modelProviders.%s 指向不存在的提供商：%s", code, id))
			continue
		}
		bindings[code] = id
	}
	if len(bindings) > 0 {
		*migrations = append(*migrations, "已把 modelProviders 并入模型 code（提供商/模型名），该字段不再需要")
	}
	return bindings
}

func readContext(item object, path string, errors *[]string) model.ContextConfig {
	rawValue, present := item["context"]
	if present {
		if _, ok := isObject(rawValue); !ok {
			*errors = append(*errors, path+".context 必须是对象")
		}
	}
	raw, ok := isObject(rawValue)
	if !ok {
		raw = object{}
	}
	window := int(readNumber(raw, "window", DefaultContextWindow, 1000, 10_000_000, path+".context.window", errors))
	onOverflow := model.OverflowReject
	switch raw["onOverflow"] {
	case "reject":
		onOverflow = model.OverflowReject
	case "passthrough":
		onOverflow = model.OverflowPassthrough
	case nil:
	default:
		if _, present := raw["onOverflow"]; present {
			*errors = append(*errors, path+".context.onOverflow 只能是 reject / passthrough")
		}
	}
	return model.ContextConfig{Window: window, OnOverflow: onOverflow}
}

func normalizeChainModel(value string, providers []model.ProviderConfig, bindings map[string]string, fallbackProvider string) (string, bool) {
	if parsed, ok := core.SplitModelCode(value, providers); ok {
		return core.JoinModelCode(parsed.Provider, parsed.Upstream), true
	}
	trimmed := strings.TrimSpace(value)
	provider := bindings[trimmed]
	if provider == "" {
		provider = fallbackProvider
	}
	return core.JoinModelCode(provider, value), false
}

func readChain(source object, providers []model.ProviderConfig, bindings map[string]string, fallbackProvider string, errors, warnings, migrations *[]string) model.ChainConfig {
	var raw object
	if value, present := source["chain"]; present {
		if obj, ok := isObject(value); ok {
			raw = obj
		} else {
			*errors = append(*errors, "chain 必须是对象")
		}
	} else if list, ok := source["chains"].([]any); ok {
		var items []object
		for _, entry := range list {
			if obj, ok := isObject(entry); ok {
				items = append(items, obj)
			}
		}
		if len(items) > 0 {
			for _, item := range items {
				if readString(item, "purpose", "") == "default" {
					raw = item
					break
				}
			}
			if raw == nil {
				raw = items[0]
			}
			*migrations = append(*migrations, "已把多条模型链合并为单条模型列表（保留默认链的模型与上下文设置）")
		}
	} else if _, present := source["chains"]; present {
		*errors = append(*errors, "chains 必须是数组")
	}

	if raw == nil {
		return model.ChainConfig{
			Models:                []string{},
			Context:               model.ContextConfig{Window: DefaultContextWindow, OnOverflow: model.OverflowReject},
			RetryDeadAfterMinutes: DefaultRetryDeadMinutes,
		}
	}

	models := []string{}
	migrated := map[string]bool{}
	normalize := func(value string, mark bool) string {
		code, parsed := normalizeChainModel(value, providers, bindings, fallbackProvider)
		if !parsed && mark {
			migrated[strings.TrimSpace(value)] = true
		}
		return code
	}

	if rawModels, present := raw["models"]; present {
		list, ok := rawModels.([]any)
		if !ok {
			*errors = append(*errors, "chain.models 必须是数组")
		} else {
			for _, entry := range list {
				text, ok := entry.(string)
				if !ok || strings.TrimSpace(text) == "" {
					*warnings = append(*warnings, "chain.models 忽略了非法条目")
					continue
				}
				code := normalize(text, true)
				if !contains(models, code) {
					models = append(models, code)
				}
			}
		}
	}

	if legacyActive := readString(raw, "active", ""); legacyActive != "" {
		active := normalize(legacyActive, false)
		position := indexOf(models, active)
		if position < 0 {
			*warnings = append(*warnings, fmt.Sprintf("chain.active 已废弃，已忽略不在模型列表里的 %s", active))
		} else {
			if position > 0 {
				models = append([]string{active}, append(models[:position], models[position+1:]...)...)
			}
			*migrations = append(*migrations, fmt.Sprintf("chain.active 已废弃，已把 %s 移到列表首位", active))
		}
	}

	if len(migrated) > 0 {
		*migrations = append(*migrations, fmt.Sprintf("已为 %d 个模型补全提供商标识", len(migrated)))
	}

	disabled := []string{}
	if rawDisabled, present := raw["disabled"]; present && rawDisabled != nil {
		if list, ok := rawDisabled.([]any); !ok {
			*errors = append(*errors, "chain.disabled 必须是数组")
		} else {
			for _, entry := range list {
				text, ok := entry.(string)
				if !ok || strings.TrimSpace(text) == "" {
					*warnings = append(*warnings, "chain.disabled 忽略了非法条目")
					continue
				}
				code := normalize(text, false)
				if !contains(models, code) {
					*warnings = append(*warnings, fmt.Sprintf("chain.disabled 忽略了不在模型列表里的 %s", code))
					continue
				}
				if !contains(disabled, code) {
					disabled = append(disabled, code)
				}
			}
		}
	}

	return model.ChainConfig{
		Models:                models,
		Context:               readContext(raw, "chain", errors),
		RetryDeadAfterMinutes: int(readNumber(raw, "retryDeadAfterMinutes", DefaultRetryDeadMinutes, 0, 1_000_000, "chain.retryDeadAfterMinutes", errors)),
		Disabled:              disabled,
	}
}

func readModelApis(source object, chain model.ChainConfig, providers []model.ProviderConfig, bindings map[string]string, errors, migrations *[]string) map[string][]model.ApiKind {
	result := map[string][]model.ApiKind{}
	raw, present := source["modelApis"]
	if present {
		if _, ok := isObject(raw); !ok {
			*errors = append(*errors, "modelApis 必须是对象")
		}
	}
	if obj, ok := isObject(raw); ok {
		dropped := 0
		for rawCode, rawValue := range obj {
			rawKey := strings.TrimSpace(rawCode)
			if rawKey == "" {
				*errors = append(*errors, "modelApis 包含空的模型 code")
				continue
			}
			var code string
			if parsed, ok := core.SplitModelCode(rawKey, providers); ok {
				code = core.JoinModelCode(parsed.Provider, parsed.Upstream)
			} else if binding, ok := bindings[rawKey]; ok {
				code = core.JoinModelCode(binding, rawKey)
			} else if contains(chain.Models, rawKey) {
				code = rawKey
			} else {
				dropped++
				continue
			}
			list, ok := rawValue.([]any)
			if !ok {
				*errors = append(*errors, fmt.Sprintf("modelApis.%s 必须是数组", rawKey))
				continue
			}
			var apis []model.ApiKind
			for _, entry := range list {
				text, isString := entry.(string)
				if !isString || !model.IsApiKind(text) {
					*errors = append(*errors, fmt.Sprintf("modelApis.%s 包含未知 API：%s", rawKey, formatValue(entry)))
					continue
				}
				if !containsApi(apis, model.ApiKind(text)) {
					apis = append(apis, model.ApiKind(text))
				}
			}
			if len(apis) == 0 {
				*errors = append(*errors, fmt.Sprintf("modelApis.%s 至少需要一个 API", rawKey))
				continue
			}
			canonical := make([]model.ApiKind, 0, len(apis))
			for _, kind := range model.ApiKinds {
				if containsApi(apis, kind) {
					canonical = append(canonical, kind)
				}
			}
			result[code] = canonical
		}
		if dropped > 0 {
			*migrations = append(*migrations, fmt.Sprintf("modelApis 中 %d 个旧条目不指向已加入的模型，已清理", dropped))
		}
	}

	for _, code := range chain.Models {
		if _, ok := result[code]; !ok {
			result[code] = append([]model.ApiKind(nil), model.DefaultModelApis...)
		}
	}
	return result
}

var clockPattern = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

func readClock(value any) (int, bool) {
	text, ok := value.(string)
	if !ok {
		return 0, false
	}
	match := clockPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return 0, false
	}
	hour, _ := strconv.Atoi(match[1])
	minute, _ := strconv.Atoi(match[2])
	return hour*60 + minute, true
}

func readPrice(value any, present bool, path string, errors *[]string) float64 {
	if !present {
		*errors = append(*errors, path+" 不能为空")
		return 0
	}
	// Reuse readNumber's coercion rules without range messages duplication.
	number, ok := numberValue(value)
	if !ok || number < 0 || number > 1_000_000_000 {
		*errors = append(*errors, path+" 必须是非负数字")
		return 0
	}
	return number
}

func coversDay(tiers []model.PriceTier) bool {
	covered := make([]bool, 1440)
	for _, tier := range tiers {
		from, fromOK := readClock(tier.From)
		to, toOK := readClock(tier.To)
		if !fromOK {
			from = 0
		}
		if !toOK {
			to = 0
		}
		if from == to {
			return true
		}
		if from < to {
			for minute := from; minute < to; minute++ {
				covered[minute] = true
			}
		} else {
			for minute := from; minute < 1440; minute++ {
				covered[minute] = true
			}
			for minute := 0; minute < to; minute++ {
				covered[minute] = true
			}
		}
	}
	for _, ok := range covered {
		if !ok {
			return false
		}
	}
	return true
}

func readPricing(source object, providers []model.ProviderConfig, bindings map[string]string, errors, migrations *[]string) model.PricingConfig {
	pricing := model.PricingConfig{Currency: DefaultPricingCurrency, Unit: DefaultPricingUnit, PreferOffPeak: true, Models: map[string]model.ModelPricing{}}
	value, present := source["pricing"]
	if !present {
		return pricing
	}
	raw, ok := isObject(value)
	if !ok {
		*errors = append(*errors, "pricing 必须是对象")
		return pricing
	}

	defaultTimeZone := SystemTimeZone()
	missingTimeZone := 0
	if currency := readString(raw, "currency", pricing.Currency); currency != "" {
		pricing.Currency = currency
	}
	pricing.Unit = int64(readNumber(raw, "unit", float64(pricing.Unit), 1, 1_000_000_000, "pricing.unit", errors))
	pricing.PreferOffPeak = readBool(raw, "preferOffPeak", true)

	modelsRaw, hasModels := raw["models"]
	if !hasModels {
		return pricing
	}
	modelsObj, ok := isObject(modelsRaw)
	if !ok {
		*errors = append(*errors, "pricing.models 必须是对象")
		return pricing
	}

	dropped := 0
	for rawCode, rawValue := range modelsObj {
		rawKey := strings.TrimSpace(rawCode)
		path := "pricing.models." + rawKey
		if rawKey == "" {
			path = "pricing.models.(empty)"
			*errors = append(*errors, "pricing.models 包含空的模型 code")
			continue
		}
		var code string
		if parsed, ok := core.SplitModelCode(rawKey, providers); ok {
			code = core.JoinModelCode(parsed.Provider, parsed.Upstream)
		} else if binding, ok := bindings[rawKey]; ok {
			code = core.JoinModelCode(binding, rawKey)
		} else {
			dropped++
			continue
		}
		entry, ok := isObject(rawValue)
		if !ok {
			*errors = append(*errors, path+" 必须是对象")
			continue
		}
		tiersRaw, ok := entry["tiers"].([]any)
		if !ok || len(tiersRaw) == 0 {
			*errors = append(*errors, path+".tiers 必须是非空数组")
			continue
		}
		if len(tiersRaw) > 12 {
			*errors = append(*errors, path+".tiers 最多 12 档")
			continue
		}

		var tiers []model.PriceTier
		for index, tierValue := range tiersRaw {
			tierPath := fmt.Sprintf("%s.tiers[%d]", path, index)
			tier, ok := isObject(tierValue)
			if !ok {
				*errors = append(*errors, tierPath+" 必须是对象")
				continue
			}
			_, fromOK := readClock(tier["from"])
			_, toOK := readClock(tier["to"])
			if !fromOK {
				*errors = append(*errors, tierPath+".from 必须是 HH:MM")
			}
			if !toOK {
				*errors = append(*errors, tierPath+".to 必须是 HH:MM")
			}
			inputValue, inputPresent := tier["input"]
			cachedValue, cachedPresent := tier["cachedInput"]
			outputValue, outputPresent := tier["output"]
			input := readPrice(inputValue, inputPresent, tierPath+".input", errors)
			cachedInput := readPrice(cachedValue, cachedPresent, tierPath+".cachedInput", errors)
			output := readPrice(outputValue, outputPresent, tierPath+".output", errors)
			if cachedInput > input {
				*errors = append(*errors, tierPath+".cachedInput 不能高于 input")
			}
			if !fromOK || !toOK {
				continue
			}
			fromText, _ := tier["from"].(string)
			toText, _ := tier["to"].(string)
			tiers = append(tiers, model.PriceTier{
				From:        strings.TrimSpace(fromText),
				To:          strings.TrimSpace(toText),
				Input:       input,
				CachedInput: cachedInput,
				Output:      output,
			})
		}
		if len(tiers) == 0 {
			continue
		}
		if !coversDay(tiers) {
			*errors = append(*errors, path+".tiers 必须覆盖一整天（00:00–24:00），可用 from == to 表示全天")
		}

		timezone := defaultTimeZone
		timezoneRaw, hasTimezone := entry["timezone"]
		if !hasTimezone || timezoneRaw == nil {
			missingTimeZone++
		} else if text, ok := timezoneRaw.(string); ok && IsTimeZone(strings.TrimSpace(text)) {
			timezone = strings.TrimSpace(text)
		} else {
			*errors = append(*errors, path+".timezone 必须是 IANA 时区，例如 Asia/Shanghai 或 UTC")
		}
		pricing.Models[code] = model.ModelPricing{Timezone: timezone, Tiers: tiers}
	}
	if missingTimeZone > 0 {
		*migrations = append(*migrations, fmt.Sprintf("已为 %d 个模型的定价补充时区 %s（原先按网关进程的本地时区计价）", missingTimeZone, defaultTimeZone))
	}
	if dropped > 0 {
		*migrations = append(*migrations, fmt.Sprintf("pricing.models 中 %d 个条目没有对应已绑定提供商的模型，已忽略", dropped))
	}
	return pricing
}

func readResponsesChaining(source object, errors *[]string) model.ResponsesChainingConfig {
	defaults := model.ResponsesChainingConfig{Enabled: false, TTLMinutes: 120, MaxSessions: 500}
	value, present := source["responsesChaining"]
	if !present {
		return defaults
	}
	raw, ok := isObject(value)
	if !ok {
		*errors = append(*errors, "responsesChaining 必须是对象")
		return defaults
	}
	return model.ResponsesChainingConfig{
		Enabled:     readBool(raw, "enabled", defaults.Enabled),
		TTLMinutes:  int(readNumber(raw, "ttlMinutes", float64(defaults.TTLMinutes), 1, 1440, "responsesChaining.ttlMinutes", errors)),
		MaxSessions: int(readNumber(raw, "maxSessions", float64(defaults.MaxSessions), 1, 10000, "responsesChaining.maxSessions", errors)),
	}
}

// Normalize validates and migrates a decoded config document.
func Normalize(input any) NormalizeResult {
	var errors, warnings, migrations []string
	defaults := DefaultConfig()
	config := DefaultConfig()
	source, isObj := isObject(input)
	if !isObj {
		errors = append(errors, "配置根节点必须是对象")
		source = object{}
	}

	config.Host = readString(source, "host", defaults.Host)
	if config.Host == "" {
		config.Host = defaults.Host
	}
	config.Port = int(readNumber(source, "port", float64(defaults.Port), 1, 65535, "port", &errors))
	config.GatewayToken = readString(source, "gatewayToken", "")

	config.Providers, config.DefaultProvider = readProviders(source, &errors, &warnings, &migrations)
	bindings := readLegacyBindings(source, config.Providers, &errors, &migrations)
	config.Chain = readChain(source, config.Providers, bindings, config.DefaultProvider, &errors, &warnings, &migrations)
	config.ModelApis = readModelApis(source, config.Chain, config.Providers, bindings, &errors, &migrations)

	serverValue, serverPresent := source["server"]
	if serverPresent {
		if _, ok := isObject(serverValue); !ok {
			errors = append(errors, "server 必须是对象")
		}
	}
	server, ok := isObject(serverValue)
	if !ok {
		server = object{}
	}
	config.Server = model.ServerConfig{
		MaxBodyBytes: int(readNumber(server, "maxBodyBytes", float64(defaults.Server.MaxBodyBytes), 1024, 256*1024*1024, "server.maxBodyBytes", &errors)),
		Cors:         readBool(server, "cors", defaults.Server.Cors),
	}

	uiValue, uiPresent := source["ui"]
	if uiPresent {
		if _, ok := isObject(uiValue); !ok {
			errors = append(errors, "ui 必须是对象")
		}
	}
	ui, ok := isObject(uiValue)
	if !ok {
		ui = object{}
	}
	config.UI = model.UiConfig{Enabled: readBool(ui, "enabled", defaults.UI.Enabled)}

	logValue, logPresent := source["log"]
	if logPresent {
		if _, ok := isObject(logValue); !ok {
			errors = append(errors, "log 必须是对象")
		}
	}
	logBlock, ok := isObject(logValue)
	if !ok {
		logBlock = object{}
	}
	level := defaults.Log.Level
	if rawLevel, present := logBlock["level"]; present {
		if text, ok := rawLevel.(string); ok && model.IsLogLevel(text) {
			level = model.LogLevelName(text)
		} else {
			errors = append(errors, "log.level 只能是 debug / info / warn / error")
		}
	}
	config.Log = model.LogConfig{
		Level:    level,
		File:     readString(logBlock, "file", ""),
		MaxBytes: int64(readNumber(logBlock, "maxBytes", float64(defaults.Log.MaxBytes), 0, 10*1024*1024*1024, "log.maxBytes", &errors)),
		MaxFiles: int(readNumber(logBlock, "maxFiles", float64(defaults.Log.MaxFiles), 0, 100, "log.maxFiles", &errors)),
	}

	if len(config.Chain.Models) == 0 {
		warnings = append(warnings, "模型列表里还没有任何模型：配好提供商后从「模型目录」加入，或直接在 chain.models 里填写")
	}

	failoverValue, _ := source["failover"]
	failover, _ := isObject(failoverValue)
	if failover == nil {
		failover = object{}
	}
	config.Failover = model.FailoverConfig{
		MaxAttempts:        int(readNumber(failover, "maxAttempts", float64(defaults.Failover.MaxAttempts), 1, 32, "failover.maxAttempts", &errors)),
		ThrottleCooldownMs: int(readNumber(failover, "throttleCooldownMs", float64(defaults.Failover.ThrottleCooldownMs), 0, 3_600_000, "failover.throttleCooldownMs", &errors)),
	}

	config.Pricing = readPricing(source, config.Providers, bindings, &errors, &migrations)
	config.ResponsesChaining = readResponsesChaining(source, &errors)

	return NormalizeResult{
		OK:         len(errors) == 0,
		Config:     config,
		Errors:     dedupe(errors),
		Warnings:   dedupe(warnings),
		Migrations: dedupe(migrations),
	}
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func indexOf(list []string, value string) int {
	for index, item := range list {
		if item == value {
			return index
		}
	}
	return -1
}

func containsApi(list []model.ApiKind, value model.ApiKind) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func formatValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return formatNumber(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case nil:
		return "null"
	}
	return fmt.Sprint(value)
}
