package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Schallophy/literouter/internal/model"
)

// ConfigError reports a configuration document that could not be parsed or
// validated. Details carry the individual validation messages.
type ConfigError struct {
	Message string
	Details []string
}

func (e *ConfigError) Error() string {
	if len(e.Details) == 0 {
		return e.Message
	}
	return e.Message + ": " + strings.Join(e.Details, "; ")
}

func decodeJSON(text string, numbers bool) (any, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(text))
	if numbers {
		decoder.UseNumber()
	}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	// Reject trailing garbage, matching JSON.parse.
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("unexpected trailing data")
	}
	return value, nil
}

func normalizeJSONNumbers(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if number, err := typed.Float64(); err == nil {
			return number
		}
		return typed.String()
	case map[string]any:
		for key, entry := range typed {
			typed[key] = normalizeJSONNumbers(entry)
		}
		return typed
	case []any:
		for index, entry := range typed {
			typed[index] = normalizeJSONNumbers(entry)
		}
		return typed
	}
	return value
}

// ParseConfigText parses strict JSON first and falls back to JSONC.
func ParseConfigText(raw string) (any, error) {
	value, strictErr := decodeJSON(raw, true)
	if strictErr == nil {
		return normalizeJSONNumbers(value), nil
	}
	relaxed, err := decodeJSON(dropTrailingCommas(stripJSONComments(raw)), true)
	if err != nil {
		return nil, &ConfigError{Message: fmt.Sprintf("配置文件不是合法 JSON：%s", strictErr.Error())}
	}
	return normalizeJSONNumbers(relaxed), nil
}

// LoadedConfig is the result of loading config.json.
type LoadedConfig struct {
	Config     model.GatewayConfig
	Created    bool
	Warnings   []string
	Migrations []string
	Persisted  bool
}

// LoadConfig reads, normalizes and (when safe) rewrites the configuration.
func LoadConfig(path string) (LoadedConfig, error) {
	file, err := filepath.Abs(path)
	if err != nil {
		return LoadedConfig{}, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return LoadedConfig{}, err
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg := DefaultConfig()
			if err := SaveConfig(file, cfg); err != nil {
				return LoadedConfig{}, err
			}
			return LoadedConfig{Config: cfg, Created: true}, nil
		}
		return LoadedConfig{}, err
	}

	parsed, err := ParseConfigText(string(raw))
	if err != nil {
		if _, ok := err.(*ConfigError); ok {
			return LoadedConfig{}, err
		}
		return LoadedConfig{}, &ConfigError{Message: fmt.Sprintf("配置文件不是合法 JSON：%s", err.Error())}
	}

	result := Normalize(parsed)
	if !result.OK {
		return LoadedConfig{}, &ConfigError{Message: "配置文件校验失败", Details: result.Errors}
	}

	persisted := len(result.Migrations) > 0 && isStrictJSON(string(raw))
	if persisted {
		if err := SaveConfig(file, result.Config); err != nil {
			return LoadedConfig{}, err
		}
	}
	return LoadedConfig{
		Config:     result.Config,
		Warnings:   result.Warnings,
		Migrations: result.Migrations,
		Persisted:  persisted,
	}, nil
}

func isStrictJSON(raw string) bool {
	_, err := decodeJSON(raw, false)
	return err == nil
}

// SaveConfig writes strict pretty JSON atomically (temp file + rename).
func SaveConfig(path string, cfg model.GatewayConfig) error {
	file, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(cfg); err != nil {
		return err
	}
	temp := fmt.Sprintf("%s.%d.tmp", file, os.Getpid())
	if err := os.WriteFile(temp, buffer.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(temp, file)
}

// OverrideResult carries a config with environment overrides applied.
type OverrideResult struct {
	Config   model.GatewayConfig
	Warnings []string
}

func envValue(primary, legacy string) (string, bool, string) {
	if value, ok := os.LookupEnv(primary); ok {
		return value, true, primary
	}
	if value, ok := os.LookupEnv(legacy); ok {
		return value, true, primary
	}
	return "", false, primary
}

// ApplyEnvOverrides applies LITEROUTER_* environment overrides. The old
// SWITCHYARD_* names stay supported so existing launchers keep working.
func ApplyEnvOverrides(cfg model.GatewayConfig) OverrideResult {
	var warnings []string

	if value, present, _ := envValue("LITEROUTER_PORT", "SWITCHYARD_PORT"); present {
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && port > 0 && port < 65536 {
			cfg.Port = port
		} else {
			warnings = append(warnings, fmt.Sprintf("LITEROUTER_PORT 不是合法端口：%s", value))
		}
	}

	provider := ""
	if value, present, _ := envValue("LITEROUTER_PROVIDER", "SWITCHYARD_PROVIDER"); present {
		provider = strings.TrimSpace(value)
	}
	if provider == "" {
		if value, present, _ := envValue("LITEROUTER_UPSTREAM", "SWITCHYARD_UPSTREAM"); present {
			provider = strings.TrimSpace(value)
		}
	}
	if provider != "" {
		valid := false
		if parsed, err := parseHTTPURL(provider); err == nil && parsed {
			valid = true
		}
		if !valid {
			warnings = append(warnings, "LITEROUTER_PROVIDER 不是合法的 http(s) 地址，已忽略")
		} else {
			target := cfg.ProviderByID(cfg.DefaultProvider)
			if target == nil && len(cfg.Providers) > 0 {
				target = &cfg.Providers[0]
			}
			if target != nil {
				target.BaseURL = strings.TrimRight(provider, "/")
			}
		}
	}
	return OverrideResult{Config: cfg, Warnings: warnings}
}

func parseHTTPURL(value string) (bool, error) {
	if strings.HasPrefix(strings.ToLower(value), "http://") {
		return len(strings.TrimPrefix(value, "http://")) > 0, nil
	}
	if strings.HasPrefix(strings.ToLower(value), "https://") {
		return len(strings.TrimPrefix(value, "https://")) > 0, nil
	}
	return false, fmt.Errorf("unsupported scheme")
}
