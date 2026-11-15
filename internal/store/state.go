// Package store persists health, usage and caller statistics in state.json.
// The on-disk shape stays compatible with the historical gateway data.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Schallophy/literouter/internal/model"
	"github.com/Schallophy/literouter/internal/util"
)

// LegacyCallerModel is the bucket for usage recorded before per-model stats.
const LegacyCallerModel = "__legacy__"

// CatalogEntry is one upstream model discovered from a provider catalogue.
type CatalogEntry struct {
	Code     string `json:"code"`
	Upstream string `json:"upstream"`
	Family   string `json:"family"`
	Provider string `json:"provider"`
}

// ApiStat is per-protocol accounting.
type ApiStat struct {
	Requests                 int                 `json:"requests"`
	PromptTokens             int                 `json:"promptTokens"`
	CachedPromptTokens       int                 `json:"cachedPromptTokens"`
	CacheTrackedPromptTokens int                 `json:"cacheTrackedPromptTokens"`
	CompletionTokens         int                 `json:"completionTokens"`
	Cost                     float64             `json:"cost"`
	CostBreakdown            model.CostBreakdown `json:"costBreakdown"`
	LastModel                *string             `json:"lastModel"`
	LastUsedAt               *string             `json:"lastUsedAt"`
}

// Stats is the global counters block.
type Stats struct {
	StartedAt                string                    `json:"startedAt"`
	Requests                 int                       `json:"requests"`
	Failovers                int                       `json:"failovers"`
	PromptTokens             int                       `json:"promptTokens"`
	CachedPromptTokens       int                       `json:"cachedPromptTokens"`
	CacheTrackedPromptTokens int                       `json:"cacheTrackedPromptTokens"`
	CompletionTokens         int                       `json:"completionTokens"`
	Cost                     float64                   `json:"cost"`
	CostBreakdown            model.CostBreakdown       `json:"costBreakdown"`
	APIs                     map[model.ApiKind]*ApiStat `json:"apis"`
}

// CallerModelStat is one caller's usage on one model.
type CallerModelStat struct {
	Model                    *string      `json:"model"`
	Attempts                 int          `json:"attempts"`
	Requests                 int          `json:"requests"`
	Failures                 int          `json:"failures"`
	PromptTokens             int          `json:"promptTokens"`
	CachedPromptTokens       int          `json:"cachedPromptTokens"`
	CacheTrackedPromptTokens int          `json:"cacheTrackedPromptTokens"`
	CompletionTokens         int          `json:"completionTokens"`
	Cost                     float64      `json:"cost"`
	CostBreakdown            model.CostBreakdown `json:"costBreakdown"`
	FirstSeenAt              string       `json:"firstSeenAt"`
	LastSeenAt               string       `json:"lastSeenAt"`
	LastAPI                  *model.ApiKind `json:"lastApi"`
}

// CallerRecord is one caller's aggregate stats.
type CallerRecord struct {
	ID                       string                      `json:"id"`
	Label                    string                      `json:"label"`
	Requests                 int                         `json:"requests"`
	Failures                 int                         `json:"failures"`
	Failovers                int                         `json:"failovers"`
	PromptTokens             int                         `json:"promptTokens"`
	CachedPromptTokens       int                         `json:"cachedPromptTokens"`
	CacheTrackedPromptTokens int                         `json:"cacheTrackedPromptTokens"`
	CompletionTokens         int                         `json:"completionTokens"`
	Cost                     float64                     `json:"cost"`
	CostBreakdown            model.CostBreakdown         `json:"costBreakdown"`
	FirstSeenAt              string                      `json:"firstSeenAt"`
	LastSeenAt               string                      `json:"lastSeenAt"`
	LastModel                *string                     `json:"lastModel"`
	LastAPI                  *model.ApiKind              `json:"lastApi"`
	Models                   map[string]*CallerModelStat `json:"models"`
}

// CalibrationBucket is a learned prompt-token correction.
type CalibrationBucket struct {
	Ratio   float64 `json:"ratio"`
	Samples int     `json:"samples"`
}

// CalibrationRecord holds plain and tool-call observations.
type CalibrationRecord struct {
	Plain *CalibrationBucket `json:"plain,omitempty"`
	Tools *CalibrationBucket `json:"tools,omitempty"`
}

// PersistedState is the whole state.json document.
type PersistedState struct {
	Version     int                            `json:"version"`
	Models      map[string]*model.ModelRecord  `json:"models"`
	Callers     map[string]*CallerRecord       `json:"callers"`
	Calibration map[string]*CalibrationRecord  `json:"calibration"`
	Catalog     struct {
		FetchedAt *string        `json:"fetchedAt"`
		Models    []CatalogEntry `json:"models"`
	} `json:"catalog"`
	Stats *Stats `json:"stats"`
}

// UsageTokens is the subset of counters cost estimators need.
type UsageTokens struct {
	PromptTokens       int
	CachedPromptTokens int
	CompletionTokens   int
}

// OpenOptions configures StateStore.Open.
type OpenOptions struct {
	File    string
	Codes   []string
	OnError func(error)
}

// StateStore is a concurrency-safe persisted state holder.
type StateStore struct {
	mu      sync.RWMutex
	file    string
	data    *PersistedState
	timer   *time.Timer
	writeMu sync.Mutex
	onError func(error)
}

// Open loads (or recovers) state.json and ensures records for codes exist.
func Open(options OpenOptions) (*StateStore, error) {
	file, err := filepath.Abs(options.File)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	onError := options.OnError
	if onError == nil {
		onError = func(error) {}
	}
	data := blankState()
	raw, readErr := os.ReadFile(file)
	if readErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			backup := fmt.Sprintf("%s.corrupt-%d", file, time.Now().UnixMilli())
			_ = os.Rename(file, backup)
			onError(readErr)
		}
	} else {
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			backup := fmt.Sprintf("%s.corrupt-%d", file, time.Now().UnixMilli())
			_ = os.Rename(file, backup)
			onError(err)
		} else if record, ok := parsed.(map[string]any); ok {
			data = hydrate(record)
		}
	}
	store := &StateStore{file: file, data: data, onError: onError}
	store.Ensure(options.Codes)
	return store, nil
}

// Stats returns a copy of the global stats.
func (s *StateStore) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneStats(s.data.Stats)
}

// Catalog returns a copy of the cached catalogue.
func (s *StateStore) Catalog() (string, []CatalogEntry) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var fetchedAt string
	if s.data.Catalog.FetchedAt != nil {
		fetchedAt = *s.data.Catalog.FetchedAt
	}
	models := make([]CatalogEntry, len(s.data.Catalog.Models))
	copy(models, s.data.Catalog.Models)
	return fetchedAt, models
}

// Callers returns a copy of all caller records.
func (s *StateStore) Callers() []CallerRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CallerRecord, 0, len(s.data.Callers))
	for _, record := range s.data.Callers {
		out = append(out, *cloneCaller(record))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// All returns a copy of every model record.
func (s *StateStore) All() map[string]*model.ModelRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*model.ModelRecord, len(s.data.Models))
	for code, record := range s.data.Models {
		out[code] = cloneRecord(record)
	}
	return out
}

// Get returns a copy of one model record.
func (s *StateStore) Get(code string) *model.ModelRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if record, ok := s.data.Models[code]; ok {
		return cloneRecord(record)
	}
	return nil
}

// Ensure creates blank records for new chain models.
func (s *StateStore) Ensure(codes []string) {
	s.mu.Lock()
	changed := false
	for _, code := range codes {
		if code == "" {
			continue
		}
		if _, ok := s.data.Models[code]; !ok {
			s.data.Models[code] = blankRecord(code)
			changed = true
		}
	}
	if changed {
		s.saveSoonLocked()
	}
	s.mu.Unlock()
}

// PruneModels keeps only records for models still in the chain.
func (s *StateStore) PruneModels(keep []string) {
	allowed := make(map[string]bool, len(keep))
	for _, code := range keep {
		allowed[code] = true
	}
	s.mu.Lock()
	changed := false
	for code := range s.data.Models {
		if !allowed[code] {
			delete(s.data.Models, code)
			changed = true
		}
	}
	for code := range s.data.Calibration {
		if !allowed[code] {
			delete(s.data.Calibration, code)
			changed = true
		}
	}
	if changed {
		s.saveSoonLocked()
	}
	s.mu.Unlock()
}

// SetHealth updates health plus optional JSON-named patch fields.
func (s *StateStore) SetHealth(code string, health model.Health, patch map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.data.Models[code]
	if !ok {
		return
	}
	applyPatch(record, patch)
	record.Code = code
	record.Health = health
	s.saveSoonLocked()
}

func applyPatch(record *model.ModelRecord, patch map[string]any) {
	for key, value := range patch {
		switch key {
		case "reason":
			record.Reason = nullableString(value)
		case "lastErrorCode":
			record.LastErrorCode = nullableString(value)
		case "lastErrorMessage":
			record.LastErrorMessage = nullableString(value)
		case "checkedAt":
			record.CheckedAt = nullableString(value)
		case "lastUsedAt":
			record.LastUsedAt = nullableString(value)
		case "throttledUntil":
			record.ThrottledUntil = nullableString(value)
		case "lastStatusCode":
			record.LastStatusCode = nullableInt(value)
		}
	}
}

func nullableString(value any) *string {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		return &text
	}
	return nil
}

func nullableInt(value any) *int {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case float64:
		converted := int(typed)
		return &converted
	case int:
		converted := typed
		return &converted
	}
	return nil
}

// MarkUsed records a successful request on a model.
func (s *StateStore) MarkUsed(code string, usage *model.Usage, cost float64, api model.ApiKind, parts *model.CostSplit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markUsedLocked(code, usage, cost, api, parts)
	s.saveSoonLocked()
}

func (s *StateStore) markUsedLocked(code string, usage *model.Usage, cost float64, api model.ApiKind, parts *model.CostSplit) {
	now := isoNow()
	split, estimated := splitFor(cost, parts)
	if record, ok := s.data.Models[code]; ok {
		record.Health = model.HealthAlive
		record.Reason = nil
		record.LastStatusCode = nil
		record.LastErrorCode = nil
		record.LastErrorMessage = nil
		record.CheckedAt = stringPtr(now)
		record.LastUsedAt = stringPtr(now)
		record.Requests++
		record.ThrottledUntil = nil
		if usage != nil {
			record.PromptTokens += usage.PromptTokens
			record.CachedPromptTokens += usage.CachedPromptTokens
			if usage.CacheReported {
				record.CacheTrackedPromptTokens += usage.PromptTokens
			}
			record.CompletionTokens += usage.CompletionTokens
		}
		record.Cost += cost
		record.CostBreakdown = addSplit(record.CostBreakdown, split, estimated)
	}
	if usage != nil {
		stat := s.apiStat(api)
		s.data.Stats.PromptTokens += usage.PromptTokens
		s.data.Stats.CachedPromptTokens += usage.CachedPromptTokens
		if usage.CacheReported {
			s.data.Stats.CacheTrackedPromptTokens += usage.PromptTokens
		}
		s.data.Stats.CompletionTokens += usage.CompletionTokens
		s.data.Stats.Cost += cost
		s.data.Stats.CostBreakdown = addSplit(s.data.Stats.CostBreakdown, split, estimated)

		stat.PromptTokens += usage.PromptTokens
		stat.CachedPromptTokens += usage.CachedPromptTokens
		if usage.CacheReported {
			stat.CacheTrackedPromptTokens += usage.PromptTokens
		}
		stat.CompletionTokens += usage.CompletionTokens
		stat.Cost += cost
		stat.CostBreakdown = addSplit(stat.CostBreakdown, split, estimated)
	}
	stat := s.apiStat(api)
	stat.LastModel = stringPtr(code)
	stat.LastUsedAt = stringPtr(now)
}

// MarkFailure increments the failure counter for a model.
func (s *StateStore) MarkFailure(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok := s.data.Models[code]; ok {
		record.Failures++
	}
	s.saveSoonLocked()
}

// AddUsage records usage without touching health (streaming tail updates).
func (s *StateStore) AddUsage(code string, usage model.Usage, cost float64, api model.ApiKind, parts *model.CostSplit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := isoNow()
	split, estimated := splitFor(cost, parts)
	if record, ok := s.data.Models[code]; ok {
		record.LastUsedAt = stringPtr(now)
		record.PromptTokens += usage.PromptTokens
		record.CachedPromptTokens += usage.CachedPromptTokens
		if usage.CacheReported {
			record.CacheTrackedPromptTokens += usage.PromptTokens
		}
		record.CompletionTokens += usage.CompletionTokens
		record.Cost += cost
		record.CostBreakdown = addSplit(record.CostBreakdown, split, estimated)
	}
	s.data.Stats.PromptTokens += usage.PromptTokens
	s.data.Stats.CachedPromptTokens += usage.CachedPromptTokens
	if usage.CacheReported {
		s.data.Stats.CacheTrackedPromptTokens += usage.PromptTokens
	}
	s.data.Stats.CompletionTokens += usage.CompletionTokens
	s.data.Stats.Cost += cost
	s.data.Stats.CostBreakdown = addSplit(s.data.Stats.CostBreakdown, split, estimated)

	stat := s.apiStat(api)
	stat.PromptTokens += usage.PromptTokens
	stat.CachedPromptTokens += usage.CachedPromptTokens
	if usage.CacheReported {
		stat.CacheTrackedPromptTokens += usage.PromptTokens
	}
	stat.CompletionTokens += usage.CompletionTokens
	stat.Cost += cost
	stat.CostBreakdown = addSplit(stat.CostBreakdown, split, estimated)
	stat.LastModel = stringPtr(code)
	stat.LastUsedAt = stringPtr(now)
	s.saveSoonLocked()
}

// RecordCallerRequest updates per-caller request/failure counters.
func (s *StateStore) RecordCallerRequest(caller model.CallerIdentity, ok bool, failovers int, modelCode *string, api model.ApiKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := isoNow()
	record, exists := s.data.Callers[caller.ID]
	if !exists {
		record = blankCaller(caller, now)
		s.data.Callers[caller.ID] = record
	}
	record.Label = caller.Label
	record.Requests++
	if !ok {
		record.Failures++
	}
	if failovers > 0 {
		record.Failovers += failovers
	}
	record.LastSeenAt = now
	if modelCode != nil {
		record.LastModel = stringPtr(*modelCode)
	}
	record.LastAPI = apiPtr(api)
	s.saveSoonLocked()
}

// AddCallerUsage records caller token and cost totals.
func (s *StateStore) AddCallerUsage(caller model.CallerIdentity, usage *model.Usage, modelCode string, cost float64, api model.ApiKind, parts *model.CostSplit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := isoNow()
	record, exists := s.data.Callers[caller.ID]
	if !exists {
		record = blankCaller(caller, now)
		s.data.Callers[caller.ID] = record
	}
	split, estimated := splitFor(cost, parts)
	cached := 0
	tracked := 0
	if usage != nil {
		cached = usage.CachedPromptTokens
		if usage.CacheReported {
			tracked = usage.PromptTokens
		}
		record.PromptTokens += usage.PromptTokens
		record.CachedPromptTokens += cached
		record.CacheTrackedPromptTokens += tracked
		record.CompletionTokens += usage.CompletionTokens
	}
	record.Label = caller.Label
	record.Cost += cost
	record.CostBreakdown = addSplit(record.CostBreakdown, split, estimated)
	record.LastSeenAt = now
	record.LastModel = stringPtr(modelCode)
	record.LastAPI = apiPtr(api)

	bucket, ok := record.Models[modelCode]
	if !ok {
		bucket = blankCallerModel(stringPtr(modelCode), now)
		record.Models[modelCode] = bucket
	}
	bucket.Attempts++
	bucket.Requests++
	if usage != nil {
		bucket.PromptTokens += usage.PromptTokens
		bucket.CachedPromptTokens += cached
		bucket.CacheTrackedPromptTokens += tracked
		bucket.CompletionTokens += usage.CompletionTokens
	}
	bucket.Cost += cost
	bucket.CostBreakdown = addSplit(bucket.CostBreakdown, split, estimated)
	bucket.LastSeenAt = now
	bucket.LastAPI = apiPtr(api)
	s.saveSoonLocked()
}

// MarkCallerModelFailure records a failed caller attempt on a model.
func (s *StateStore) MarkCallerModelFailure(caller model.CallerIdentity, modelCode string, api model.ApiKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := isoNow()
	record, exists := s.data.Callers[caller.ID]
	if !exists {
		record = blankCaller(caller, now)
		s.data.Callers[caller.ID] = record
	}
	bucket, ok := record.Models[modelCode]
	if !ok {
		bucket = blankCallerModel(stringPtr(modelCode), now)
		record.Models[modelCode] = bucket
	}
	record.Label = caller.Label
	bucket.Attempts++
	bucket.Failures++
	bucket.LastSeenAt = now
	bucket.LastAPI = apiPtr(api)
	s.saveSoonLocked()
}

// CalibrationRatio returns the learned correction, defaulting to 1.
func (s *StateStore) CalibrationRatio(code string, hasTools bool) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record := s.data.Calibration[code]
	if record == nil {
		return 1
	}
	bucket := record.Plain
	if hasTools {
		if record.Tools != nil {
			bucket = record.Tools
		}
	} else if record.Plain == nil {
		bucket = record.Tools
	}
	if bucket == nil || bucket.Ratio <= 0 {
		return 1
	}
	return bucket.Ratio
}

// ObserveCalibration updates the learned ratio using weighted samples.
func (s *StateStore) ObserveCalibration(code string, hasTools bool, estimatedPrompt, actualPrompt int) {
	if estimatedPrompt < 100 || actualPrompt <= 0 {
		return
	}
	observed := clampRatio(float64(actualPrompt) / float64(estimatedPrompt))
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.data.Calibration[code]
	if record == nil {
		record = &CalibrationRecord{}
		s.data.Calibration[code] = record
	}
	key := "plain"
	if hasTools {
		key = "tools"
	}
	var previous *CalibrationBucket
	if key == "tools" {
		previous = record.Tools
	} else {
		previous = record.Plain
	}
	weight := math.Min(0.5, math.Max(0.02, float64(estimatedPrompt)/(float64(estimatedPrompt)+5000)))
	ratio := observed
	samples := 1
	if previous != nil && previous.Ratio > 0 {
		ratio = clampRatio(previous.Ratio + (observed-previous.Ratio)*weight)
		samples = previous.Samples + 1
	}
	bucket := &CalibrationBucket{Ratio: ratio, Samples: samples}
	if key == "tools" {
		record.Tools = bucket
	} else {
		record.Plain = bucket
	}
	s.saveSoonLocked()
}

// MigrateModelCodes merges legacy bare model names into provider-prefixed codes.
func (s *StateStore) MigrateModelCodes(resolve func(string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for code, record := range s.data.Models {
		target := resolve(code)
		if target == "" || target == code {
			continue
		}
		if existing, ok := s.data.Models[target]; !ok {
			copied := cloneRecord(record)
			copied.Code = target
			s.data.Models[target] = copied
		} else {
			keep := existing
			if newer(record.CheckedAt, existing.CheckedAt) == record.CheckedAt {
				keep = record
			}
			existing.Health = keep.Health
			existing.Reason = keep.Reason
			existing.LastErrorCode = keep.LastErrorCode
			existing.LastStatusCode = keep.LastStatusCode
			existing.LastErrorMessage = keep.LastErrorMessage
			existing.CheckedAt = keep.CheckedAt
			existing.ThrottledUntil = keep.ThrottledUntil
			if newer(existing.LastUsedAt, record.LastUsedAt) == record.LastUsedAt {
				existing.LastUsedAt = record.LastUsedAt
			}
			existing.Requests += record.Requests
			existing.Failures += record.Failures
			existing.PromptTokens += record.PromptTokens
			existing.CachedPromptTokens += record.CachedPromptTokens
			existing.CacheTrackedPromptTokens += record.CacheTrackedPromptTokens
			existing.CompletionTokens += record.CompletionTokens
			existing.Cost += record.Cost
			existing.CostBreakdown = sumBreakdowns([]model.CostBreakdown{existing.CostBreakdown, record.CostBreakdown})
		}
		delete(s.data.Models, code)
		changed = true
	}

	for code, record := range s.data.Calibration {
		target := resolve(code)
		if target == "" || target == code {
			continue
		}
		if _, ok := s.data.Calibration[target]; !ok {
			s.data.Calibration[target] = record
		}
		delete(s.data.Calibration, code)
		changed = true
	}

	for _, stat := range s.data.Stats.APIs {
		if stat.LastModel != nil {
			if target := resolve(*stat.LastModel); target != "" {
				stat.LastModel = stringPtr(target)
			}
		}
	}
	for _, caller := range s.data.Callers {
		if caller.LastModel != nil {
			if target := resolve(*caller.LastModel); target != "" {
				caller.LastModel = stringPtr(target)
			}
		}
		for key, bucket := range caller.Models {
			if key == LegacyCallerModel {
				continue
			}
			target := resolve(key)
			if target == "" || target == key {
				continue
			}
			if existing, ok := caller.Models[target]; ok {
				existing.Model = stringPtr(target)
				existing.Attempts += bucket.Attempts
				existing.Requests += bucket.Requests
				existing.Failures += bucket.Failures
				existing.PromptTokens += bucket.PromptTokens
				existing.CachedPromptTokens += bucket.CachedPromptTokens
				existing.CacheTrackedPromptTokens += bucket.CacheTrackedPromptTokens
				existing.CompletionTokens += bucket.CompletionTokens
				existing.Cost += bucket.Cost
				existing.CostBreakdown = sumBreakdowns([]model.CostBreakdown{existing.CostBreakdown, bucket.CostBreakdown})
				if parseTime(bucket.FirstSeenAt) < parseTime(existing.FirstSeenAt) {
					existing.FirstSeenAt = bucket.FirstSeenAt
				}
				if parseTime(bucket.LastSeenAt) > parseTime(existing.LastSeenAt) {
					existing.LastSeenAt = bucket.LastSeenAt
				}
			} else {
				copied := *bucket
				copied.Model = stringPtr(target)
				caller.Models[target] = &copied
			}
			delete(caller.Models, key)
			changed = true
		}
	}
	if changed {
		s.saveSoonLocked()
	}
}

// BumpRequests counts one top-level gateway request.
func (s *StateStore) BumpRequests(api model.ApiKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := isoNow()
	s.data.Stats.Requests++
	stat := s.apiStat(api)
	stat.Requests++
	stat.LastUsedAt = stringPtr(now)
	s.saveSoonLocked()
}

// UsageForRecord builds the estimator input for a model record.
func UsageForRecord(record *model.ModelRecord) UsageTokens {
	return UsageTokens{PromptTokens: record.PromptTokens, CachedPromptTokens: record.CachedPromptTokens, CompletionTokens: record.CompletionTokens}
}

// RecomputeCosts recalculates cost from stored token totals.
func (s *StateStore) RecomputeCosts(costFor func(modelCode string, usage UsageTokens) model.CostSplit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	modelTotal := 0.0
	modelParts := make([]model.CostBreakdown, 0, len(s.data.Models))
	for _, record := range s.data.Models {
		split := costFor(record.Code, UsageForRecord(record))
		record.Cost = split.Total()
		record.CostBreakdown = breakdownOf(split)
		modelTotal += record.Cost
		modelParts = append(modelParts, record.CostBreakdown)
	}

	for _, caller := range s.data.Callers {
		if len(caller.Models) == 0 {
			modelCode := ""
			if caller.LastModel != nil {
				modelCode = *caller.LastModel
			}
			split := costFor(modelCode, UsageTokens{caller.PromptTokens, caller.CachedPromptTokens, caller.CompletionTokens})
			caller.Cost = split.Total()
			caller.CostBreakdown = breakdownOf(split)
			continue
		}
		callerTotal := 0.0
		callerParts := make([]model.CostBreakdown, 0, len(caller.Models))
		for _, bucket := range caller.Models {
			modelCode := ""
			if bucket.Model != nil {
				modelCode = *bucket.Model
			} else if caller.LastModel != nil {
				modelCode = *caller.LastModel
			}
			split := costFor(modelCode, UsageTokens{bucket.PromptTokens, bucket.CachedPromptTokens, bucket.CompletionTokens})
			bucket.Cost = split.Total()
			bucket.CostBreakdown = breakdownOf(split)
			callerTotal += bucket.Cost
			callerParts = append(callerParts, bucket.CostBreakdown)
		}
		caller.Cost = callerTotal
		caller.CostBreakdown = sumBreakdowns(callerParts)
	}

	for _, stat := range s.data.Stats.APIs {
		modelCode := ""
		if stat.LastModel != nil {
			modelCode = *stat.LastModel
		}
		split := costFor(modelCode, UsageTokens{stat.PromptTokens, stat.CachedPromptTokens, stat.CompletionTokens})
		stat.Cost = split.Total()
		stat.CostBreakdown = breakdownOf(split)
	}

	if len(s.data.Callers) > 0 {
		total := 0.0
		parts := make([]model.CostBreakdown, 0, len(s.data.Callers))
		for _, caller := range s.data.Callers {
			total += caller.Cost
			parts = append(parts, caller.CostBreakdown)
		}
		s.data.Stats.Cost = total
		s.data.Stats.CostBreakdown = sumBreakdowns(parts)
	} else {
		s.data.Stats.Cost = modelTotal
		s.data.Stats.CostBreakdown = sumBreakdowns(modelParts)
	}
	s.saveSoonLocked()
}

// BackfillUsageDetails fills cost breakdowns and caller/model detail for legacy
// data. It returns whether anything changed.
func (s *StateStore) BackfillUsageDetails(estimate func(modelCode string, usage UsageTokens) *model.CostSplit) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false

	for _, record := range s.data.Models {
		if fillBreakdown(&record.CostBreakdown, record.Cost, func() *model.CostSplit {
			return estimate(record.Code, UsageForRecord(record))
		}) {
			changed = true
		}
	}

	var soleCaller *CallerRecord
	if len(s.data.Callers) == 1 {
		for _, caller := range s.data.Callers {
			soleCaller = caller
		}
	}
	for _, caller := range s.data.Callers {
		if len(caller.Models) == 0 && (caller.PromptTokens > 0 || caller.CompletionTokens > 0 || caller.Cost > 0) {
			var records []*model.ModelRecord
			if soleCaller == caller {
				for _, record := range s.data.Models {
					records = append(records, record)
				}
			}
			s.seedCallerModels(caller, records)
			changed = true
		}
		for _, bucket := range caller.Models {
			modelCode := ""
			if bucket.Model != nil {
				modelCode = *bucket.Model
			} else if caller.LastModel != nil {
				modelCode = *caller.LastModel
			}
			if fillBreakdown(&bucket.CostBreakdown, bucket.Cost, func() *model.CostSplit {
				if modelCode == "" {
					return nil
				}
				return estimate(modelCode, UsageTokens{bucket.PromptTokens, bucket.CachedPromptTokens, bucket.CompletionTokens})
			}) {
				changed = true
			}
		}
		if caller.CostBreakdown.Total() <= 0 && caller.Cost > 0 {
			parts := make([]model.CostBreakdown, 0, len(caller.Models))
			for _, bucket := range caller.Models {
				parts = append(parts, bucket.CostBreakdown)
			}
			fromModels := sumBreakdowns(parts)
			if math.Abs(fromModels.Total()-caller.Cost) <= 1e-6 && fromModels.Total() > 0 {
				caller.CostBreakdown = fromModels
			} else {
				modelCode := ""
				if caller.LastModel != nil {
					modelCode = *caller.LastModel
				}
				caller.CostBreakdown = scaleSplit(estimate(modelCode, UsageTokens{caller.PromptTokens, caller.CachedPromptTokens, caller.CompletionTokens}), caller.Cost)
			}
			changed = true
		}
	}

	for _, stat := range s.data.Stats.APIs {
		modelCode := ""
		if stat.LastModel != nil {
			modelCode = *stat.LastModel
		}
		if fillBreakdown(&stat.CostBreakdown, stat.Cost, func() *model.CostSplit {
			return estimate(modelCode, UsageTokens{stat.PromptTokens, stat.CachedPromptTokens, stat.CompletionTokens})
		}) {
			changed = true
		}
	}
	if s.data.Stats.CostBreakdown.Total() <= 0 && s.data.Stats.Cost > 0 {
		callerParts := make([]model.CostBreakdown, 0, len(s.data.Callers))
		for _, caller := range s.data.Callers {
			callerParts = append(callerParts, caller.CostBreakdown)
		}
		fromCallers := sumBreakdowns(callerParts)
		if math.Abs(fromCallers.Total()-s.data.Stats.Cost) <= 1e-6 && fromCallers.Total() > 0 {
			s.data.Stats.CostBreakdown = fromCallers
		} else {
			modelCode := s.latestModelLocked()
			var split *model.CostSplit
			if modelCode != "" {
				split = estimate(modelCode, UsageTokens{s.data.Stats.PromptTokens, s.data.Stats.CachedPromptTokens, s.data.Stats.CompletionTokens})
			}
			s.data.Stats.CostBreakdown = scaleSplit(split, s.data.Stats.Cost)
		}
		changed = true
	}

	if changed {
		s.saveSoonLocked()
	}
	return changed
}

func fillBreakdown(breakdown *model.CostBreakdown, targetCost float64, estimate func() *model.CostSplit) bool {
	if breakdown.Total() > 0 || targetCost <= 0 {
		return false
	}
	*breakdown = scaleSplit(estimate(), targetCost)
	return true
}

func (s *StateStore) seedCallerModels(caller *CallerRecord, records []*model.ModelRecord) {
	usable := make([]*model.ModelRecord, 0, len(records))
	for _, record := range records {
		if record.PromptTokens > 0 || record.CompletionTokens > 0 || record.Cost > 0 {
			usable = append(usable, record)
		}
	}
	if len(usable) > 0 {
		var prompt, cached, tracked, completion int
		cost := 0.0
		for _, record := range usable {
			prompt += record.PromptTokens
			cached += record.CachedPromptTokens
			tracked += record.CacheTrackedPromptTokens
			completion += record.CompletionTokens
			cost += record.Cost
		}
		fits := float64(prompt) <= float64(caller.PromptTokens)+1e-6 &&
			float64(cached) <= float64(caller.CachedPromptTokens)+1e-6 &&
			float64(tracked) <= float64(caller.CacheTrackedPromptTokens)+1e-6 &&
			float64(completion) <= float64(caller.CompletionTokens)+1e-6 &&
			cost <= caller.Cost+1e-6
		if fits {
			for _, record := range usable {
				firstSeen := caller.FirstSeenAt
				if record.LastUsedAt != nil {
					firstSeen = *record.LastUsedAt
				}
				lastSeen := caller.LastSeenAt
				if record.LastUsedAt != nil {
					lastSeen = *record.LastUsedAt
				}
				caller.Models[record.Code] = &CallerModelStat{
					Model:                    stringPtr(record.Code),
					Attempts:                 record.Requests + record.Failures,
					Requests:                 record.Requests,
					Failures:                 record.Failures,
					PromptTokens:             record.PromptTokens,
					CachedPromptTokens:       record.CachedPromptTokens,
					CacheTrackedPromptTokens: record.CacheTrackedPromptTokens,
					CompletionTokens:         record.CompletionTokens,
					Cost:                     record.Cost,
					CostBreakdown:            record.CostBreakdown,
					FirstSeenAt:              firstSeen,
					LastSeenAt:               lastSeen,
					LastAPI:                  caller.LastAPI,
				}
			}
			remainingPrompt := maxInt(0, caller.PromptTokens-prompt)
			remainingCached := maxInt(0, caller.CachedPromptTokens-cached)
			remainingTracked := maxInt(0, caller.CacheTrackedPromptTokens-tracked)
			remainingCompletion := maxInt(0, caller.CompletionTokens-completion)
			remainingCost := math.Max(0, caller.Cost-cost)
			if remainingPrompt > 0 || remainingCompletion > 0 || remainingCost > 1e-9 {
				caller.Models[LegacyCallerModel] = &CallerModelStat{
					Attempts:                 caller.Requests,
					Requests:                 caller.Requests,
					Failures:                 caller.Failures,
					PromptTokens:             remainingPrompt,
					CachedPromptTokens:       remainingCached,
					CacheTrackedPromptTokens: remainingTracked,
					CompletionTokens:         remainingCompletion,
					Cost:                     remainingCost,
					CostBreakdown:            zeroBreakdown(true),
					FirstSeenAt:              caller.FirstSeenAt,
					LastSeenAt:               caller.LastSeenAt,
					LastAPI:                  caller.LastAPI,
				}
			}
			return
		}
	}

	caller.Models[LegacyCallerModel] = &CallerModelStat{
		Attempts:                 caller.Requests,
		Requests:                 caller.Requests,
		Failures:                 caller.Failures,
		PromptTokens:             caller.PromptTokens,
		CachedPromptTokens:       caller.CachedPromptTokens,
		CacheTrackedPromptTokens: caller.CacheTrackedPromptTokens,
		CompletionTokens:         caller.CompletionTokens,
		Cost:                     caller.Cost,
		CostBreakdown:            zeroBreakdown(true),
		FirstSeenAt:              caller.FirstSeenAt,
		LastSeenAt:               caller.LastSeenAt,
		LastAPI:                  caller.LastAPI,
	}
}

func (s *StateStore) latestModelLocked() string {
	bestAt := int64(-1)
	best := ""
	for _, stat := range s.data.Stats.APIs {
		if stat.LastModel == nil {
			continue
		}
		at := int64(0)
		if stat.LastUsedAt != nil {
			at = parseTime(*stat.LastUsedAt)
		}
		if at > bestAt {
			bestAt = at
			best = *stat.LastModel
		}
	}
	if best != "" {
		return best
	}
	for _, caller := range s.data.Callers {
		if caller.LastModel != nil {
			return *caller.LastModel
		}
	}
	return ""
}

// ResetUsage clears every usage counter while preserving health state.
func (s *StateStore) ResetUsage() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Stats = blankStats()
	for _, record := range s.data.Models {
		record.Requests = 0
		record.Failures = 0
		record.PromptTokens = 0
		record.CachedPromptTokens = 0
		record.CacheTrackedPromptTokens = 0
		record.CompletionTokens = 0
		record.Cost = 0
		record.CostBreakdown = zeroBreakdown(false)
	}
	s.data.Callers = map[string]*CallerRecord{}
	s.saveSoonLocked()
}

// BumpFailovers adds to the global failover counter.
func (s *StateStore) BumpFailovers(count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Stats.Failovers += count
	s.saveSoonLocked()
}

// SetCatalog stores a freshly fetched provider catalogue.
func (s *StateStore) SetCatalog(entries []struct{ ID, Provider string }) {
	byCode := map[string]CatalogEntry{}
	for _, entry := range entries {
		upstream := strings.TrimSpace(entry.ID)
		provider := strings.TrimSpace(entry.Provider)
		if upstream == "" || provider == "" {
			continue
		}
		code := provider + "/" + upstream
		if _, ok := byCode[code]; ok {
			continue
		}
		byCode[code] = CatalogEntry{Code: code, Upstream: upstream, Family: util.FamilyOf(upstream), Provider: provider}
	}
	models := make([]CatalogEntry, 0, len(byCode))
	for _, entry := range byCode {
		models = append(models, entry)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Family != models[j].Family {
			return models[i].Family < models[j].Family
		}
		return models[i].Code < models[j].Code
	})
	now := isoNow()
	s.mu.Lock()
	s.data.Catalog.FetchedAt = &now
	s.data.Catalog.Models = models
	s.saveSoonLocked()
	s.mu.Unlock()
}

// SaveSoon schedules a debounced write.
func (s *StateStore) SaveSoon() {
	s.mu.Lock()
	s.saveSoonLocked()
	s.mu.Unlock()
}

func (s *StateStore) saveSoonLocked() {
	if s.timer != nil {
		return
	}
	s.timer = time.AfterFunc(300*time.Millisecond, func() {
		s.mu.Lock()
		s.timer = nil
		snapshot := cloneState(s.data)
		s.mu.Unlock()
		if err := s.writeSnapshot(snapshot); err != nil {
			s.onError(err)
		}
	})
}

// Flush cancels the debounce timer and writes synchronously.
func (s *StateStore) Flush() error {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	snapshot := cloneState(s.data)
	s.mu.Unlock()
	return s.writeSnapshot(snapshot)
}

func (s *StateStore) writeSnapshot(snapshot *PersistedState) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	payload, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temp := fmt.Sprintf("%s.%d.tmp", s.file, os.Getpid())
	if err := os.WriteFile(temp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, s.file)
}

func (s *StateStore) apiStat(api model.ApiKind) *ApiStat {
	stat, ok := s.data.Stats.APIs[api]
	if !ok || stat == nil {
		stat = blankAPIStat()
		s.data.Stats.APIs[api] = stat
	}
	return stat
}

func isoNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func parseTime(value string) int64 {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

func stringPtr(value string) *string { return &value }

func apiPtr(api model.ApiKind) *model.ApiKind {
	value := api
	return &value
}

func zeroSplitValue() model.CostSplit { return model.CostSplit{} }

func zeroBreakdown(estimated bool) model.CostBreakdown {
	return model.CostBreakdown{CostSplit: zeroSplitValue(), Estimated: estimated}
}

func breakdownOf(split model.CostSplit) model.CostBreakdown {
	return model.CostBreakdown{CostSplit: split, Estimated: true}
}

func addSplit(base model.CostBreakdown, parts model.CostSplit, estimated bool) model.CostBreakdown {
	return model.CostBreakdown{
		CostSplit: model.CostSplit{
			Input:       base.Input + parts.Input,
			CachedInput: base.CachedInput + parts.CachedInput,
			Output:      base.Output + parts.Output,
			CacheSaved:  base.CacheSaved + parts.CacheSaved,
		},
		Estimated: base.Estimated || estimated,
	}
}

func sumBreakdowns(parts []model.CostBreakdown) model.CostBreakdown {
	out := zeroBreakdown(false)
	for _, part := range parts {
		out.Input += part.Input
		out.CachedInput += part.CachedInput
		out.Output += part.Output
		out.CacheSaved += part.CacheSaved
		out.Estimated = out.Estimated || part.Estimated
	}
	return out
}

func fallbackSplit(cost float64) model.CostSplit {
	if cost > 0 {
		return model.CostSplit{Input: cost}
	}
	return model.CostSplit{}
}

func scaleSplit(parts *model.CostSplit, targetCost float64) model.CostBreakdown {
	if targetCost <= 0 {
		return zeroBreakdown(true)
	}
	if parts == nil {
		split := fallbackSplit(targetCost)
		return model.CostBreakdown{CostSplit: split, Estimated: true}
	}
	raw := parts.Total()
	if raw <= 0 {
		split := fallbackSplit(targetCost)
		return model.CostBreakdown{CostSplit: split, Estimated: true}
	}
	factor := targetCost / raw
	return model.CostBreakdown{
		CostSplit: model.CostSplit{
			Input:       parts.Input * factor,
			CachedInput: parts.CachedInput * factor,
			Output:      parts.Output * factor,
			CacheSaved:  parts.CacheSaved * factor,
		},
		Estimated: true,
	}
}

func splitFor(cost float64, parts *model.CostSplit) (model.CostSplit, bool) {
	if parts != nil {
		return *parts, false
	}
	return fallbackSplit(cost), cost > 0
}

func blankRecord(code string) *model.ModelRecord {
	return &model.ModelRecord{
		Code:          code,
		Family:        util.FamilyOf(code),
		Health:        model.HealthUnknown,
		CostBreakdown: zeroBreakdown(false),
	}
}

func blankAPIStat() *ApiStat {
	return &ApiStat{CostBreakdown: zeroBreakdown(false)}
}

func blankApis() map[model.ApiKind]*ApiStat {
	return map[model.ApiKind]*ApiStat{
		model.ApiChat:        blankAPIStat(),
		model.ApiCompletions: blankAPIStat(),
		model.ApiResponses:   blankAPIStat(),
		model.ApiEmbeddings:  blankAPIStat(),
	}
}

func blankStats() *Stats {
	return &Stats{StartedAt: isoNow(), CostBreakdown: zeroBreakdown(false), APIs: blankApis()}
}

func blankCallerModel(modelCode *string, at string) *CallerModelStat {
	return &CallerModelStat{Model: modelCode, CostBreakdown: zeroBreakdown(false), FirstSeenAt: at, LastSeenAt: at}
}

func blankCaller(caller model.CallerIdentity, at string) *CallerRecord {
	return &CallerRecord{
		ID:            caller.ID,
		Label:         caller.Label,
		CostBreakdown: zeroBreakdown(false),
		FirstSeenAt:   at,
		LastSeenAt:    at,
		Models:        map[string]*CallerModelStat{},
	}
}

func blankState() *PersistedState {
	state := &PersistedState{
		Version:     2,
		Models:      map[string]*model.ModelRecord{},
		Callers:     map[string]*CallerRecord{},
		Calibration: map[string]*CalibrationRecord{},
		Stats:       blankStats(),
	}
	state.Catalog.Models = []CatalogEntry{}
	return state
}

func cloneRecord(record *model.ModelRecord) *model.ModelRecord {
	copied := *record
	return &copied
}

func cloneCaller(caller *CallerRecord) *CallerRecord {
	copied := *caller
	copied.Models = make(map[string]*CallerModelStat, len(caller.Models))
	for key, bucket := range caller.Models {
		bucketCopy := *bucket
		copied.Models[key] = &bucketCopy
	}
	return &copied
}

func cloneStats(stats *Stats) Stats {
	copied := *stats
	copied.APIs = make(map[model.ApiKind]*ApiStat, len(stats.APIs))
	for kind, stat := range stats.APIs {
		statCopy := *stat
		copied.APIs[kind] = &statCopy
	}
	return copied
}

func cloneState(state *PersistedState) *PersistedState {
	copied := &PersistedState{
		Version:     state.Version,
		Models:      make(map[string]*model.ModelRecord, len(state.Models)),
		Callers:     make(map[string]*CallerRecord, len(state.Callers)),
		Calibration: make(map[string]*CalibrationRecord, len(state.Calibration)),
		Stats:       func() *Stats { s := cloneStats(state.Stats); return &s }(),
	}
	for code, record := range state.Models {
		copied.Models[code] = cloneRecord(record)
	}
	for id, caller := range state.Callers {
		copied.Callers[id] = cloneCaller(caller)
	}
	for code, record := range state.Calibration {
		copiedRecord := *record
		copied.Calibration[code] = &copiedRecord
	}
	copied.Catalog.FetchedAt = state.Catalog.FetchedAt
	copied.Catalog.Models = append([]CatalogEntry(nil), state.Catalog.Models...)
	return copied
}

func newer(left, right *string) *string {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	if parseTime(*left) >= parseTime(*right) {
		return left
	}
	return right
}

func clampRatio(value float64) float64 {
	return math.Min(5, math.Max(0.2, value))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func numberOr(value any, fallback int) int {
	if number, ok := value.(float64); ok && !math.IsNaN(number) && !math.IsInf(number, 0) {
		return int(number)
	}
	return fallback
}

func floatOr(value any, fallback float64) float64 {
	if number, ok := value.(float64); ok && !math.IsNaN(number) && !math.IsInf(number, 0) {
		return number
	}
	return fallback
}

func stringOr(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}

func stringPtrOr(value any) *string {
	if text, ok := value.(string); ok {
		return &text
	}
	return nil
}

func apiKindOrNil(value any) *model.ApiKind {
	if text, ok := value.(string); ok && model.IsApiKind(text) {
		kind := model.ApiKind(text)
		return &kind
	}
	return nil
}

func readBreakdown(value any) model.CostBreakdown {
	record, ok := value.(map[string]any)
	if !ok {
		return zeroBreakdown(false)
	}
	estimated, _ := record["estimated"].(bool)
	return model.CostBreakdown{
		CostSplit: model.CostSplit{
			Input:       floatOr(record["input"], 0),
			CachedInput: floatOr(record["cachedInput"], 0),
			Output:      floatOr(record["output"], 0),
			CacheSaved:  floatOr(record["cacheSaved"], 0),
		},
		Estimated: estimated,
	}
}

func hydrate(source map[string]any) *PersistedState {
	base := blankState()
	startedAt := base.Stats.StartedAt

	if stats, ok := source["stats"].(map[string]any); ok {
		base.Stats = &Stats{
			StartedAt:                stringOr(stats["startedAt"], startedAt),
			Requests:                 numberOr(stats["requests"], 0),
			Failovers:                numberOr(stats["failovers"], 0),
			PromptTokens:             numberOr(stats["promptTokens"], 0),
			CachedPromptTokens:       numberOr(stats["cachedPromptTokens"], 0),
			CacheTrackedPromptTokens: numberOr(stats["cacheTrackedPromptTokens"], 0),
			CompletionTokens:         numberOr(stats["completionTokens"], 0),
			Cost:                     floatOr(stats["cost"], 0),
			CostBreakdown:            readBreakdown(stats["costBreakdown"]),
			APIs:                     hydrateAPIs(stats["apis"]),
		}
		startedAt = base.Stats.StartedAt
	}

	if callers, ok := source["callers"].(map[string]any); ok {
		for id, raw := range callers {
			record, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			label := stringOr(record["label"], "")
			if label == "" {
				label = id
			}
			base.Callers[id] = &CallerRecord{
				ID:                       id,
				Label:                    label,
				Requests:                 numberOr(record["requests"], 0),
				Failures:                 numberOr(record["failures"], 0),
				Failovers:                numberOr(record["failovers"], 0),
				PromptTokens:             numberOr(record["promptTokens"], 0),
				CachedPromptTokens:       numberOr(record["cachedPromptTokens"], 0),
				CacheTrackedPromptTokens: numberOr(record["cacheTrackedPromptTokens"], 0),
				CompletionTokens:         numberOr(record["completionTokens"], 0),
				Cost:                     floatOr(record["cost"], 0),
				CostBreakdown:            readBreakdown(record["costBreakdown"]),
				FirstSeenAt:              stringOr(record["firstSeenAt"], startedAt),
				LastSeenAt:               stringOr(record["lastSeenAt"], startedAt),
				LastModel:                stringPtrOr(record["lastModel"]),
				LastAPI:                  apiKindOrNil(record["lastApi"]),
				Models:                   hydrateCallerModels(record["models"], startedAt),
			}
		}
	}

	if models, ok := source["models"].(map[string]any); ok {
		for code, raw := range models {
			record, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				continue
			}
			base.Models[code] = blankRecord(code)
			_ = json.Unmarshal(encoded, base.Models[code])
			base.Models[code].CostBreakdown = readBreakdown(record["costBreakdown"])
			if base.Models[code].Code == "" {
				base.Models[code].Code = code
			}
			if base.Models[code].Family == "" {
				base.Models[code].Family = util.FamilyOf(code)
			}
			if base.Models[code].Health == "" {
				base.Models[code].Health = model.HealthUnknown
			}
		}
	}

	if calibration, ok := source["calibration"].(map[string]any); ok {
		for code, raw := range calibration {
			record, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			entry := &CalibrationRecord{}
			if bucket := bucketFrom(record["plain"]); bucket != nil {
				entry.Plain = bucket
			}
			if bucket := bucketFrom(record["tools"]); bucket != nil {
				entry.Tools = bucket
			}
			if entry.Plain != nil || entry.Tools != nil {
				base.Calibration[code] = entry
			}
		}
	}

	if catalog, ok := source["catalog"].(map[string]any); ok {
		list, _ := catalog["models"].([]any)
		seen := map[string]bool{}
		models := make([]CatalogEntry, 0, len(list))
		for _, item := range list {
			raw, ok := item.(map[string]any)
			if !ok {
				continue
			}
			upstream := strings.TrimSpace(stringOr(raw["upstream"], stringOr(raw["id"], "")))
			if upstream == "" {
				continue
			}
			provider := strings.TrimSpace(stringOr(raw["provider"], ""))
			code := strings.TrimSpace(stringOr(raw["code"], ""))
			if code == "" {
				if provider != "" {
					code = provider + "/" + upstream
				} else {
					code = upstream
				}
			}
			if seen[code] {
				continue
			}
			seen[code] = true
			family := stringOr(raw["family"], "")
			if family == "" {
				family = util.FamilyOf(upstream)
			}
			models = append(models, CatalogEntry{Code: code, Upstream: upstream, Family: family, Provider: provider})
		}
		base.Catalog.FetchedAt = stringPtrOr(catalog["fetchedAt"])
		base.Catalog.Models = models
	}

	return base
}

func hydrateAPIs(raw any) map[model.ApiKind]*ApiStat {
	apis := blankApis()
	record, ok := raw.(map[string]any)
	if !ok {
		return apis
	}
	for _, kind := range model.ApiKinds {
		value, ok := record[string(kind)].(map[string]any)
		if !ok {
			continue
		}
		apis[kind] = &ApiStat{
			Requests:                 numberOr(value["requests"], 0),
			PromptTokens:             numberOr(value["promptTokens"], 0),
			CachedPromptTokens:       numberOr(value["cachedPromptTokens"], 0),
			CacheTrackedPromptTokens: numberOr(value["cacheTrackedPromptTokens"], 0),
			CompletionTokens:         numberOr(value["completionTokens"], 0),
			Cost:                     floatOr(value["cost"], 0),
			CostBreakdown:            readBreakdown(value["costBreakdown"]),
			LastModel:                stringPtrOr(value["lastModel"]),
			LastUsedAt:               stringPtrOr(value["lastUsedAt"]),
		}
	}
	return apis
}

func hydrateCallerModels(raw any, fallbackAt string) map[string]*CallerModelStat {
	models := map[string]*CallerModelStat{}
	record, ok := raw.(map[string]any)
	if !ok {
		return models
	}
	for key, value := range record {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		var modelCode *string
		if key != LegacyCallerModel {
			if text, ok := item["model"].(string); ok {
				modelCode = &text
			} else {
				modelCode = stringPtr(key)
			}
		}
		models[key] = &CallerModelStat{
			Model:                    modelCode,
			Attempts:                 numberOr(item["attempts"], 0),
			Requests:                 numberOr(item["requests"], 0),
			Failures:                 numberOr(item["failures"], 0),
			PromptTokens:             numberOr(item["promptTokens"], 0),
			CachedPromptTokens:       numberOr(item["cachedPromptTokens"], 0),
			CacheTrackedPromptTokens: numberOr(item["cacheTrackedPromptTokens"], 0),
			CompletionTokens:         numberOr(item["completionTokens"], 0),
			Cost:                     floatOr(item["cost"], 0),
			CostBreakdown:            readBreakdown(item["costBreakdown"]),
			FirstSeenAt:              stringOr(item["firstSeenAt"], fallbackAt),
			LastSeenAt:               stringOr(item["lastSeenAt"], fallbackAt),
			LastAPI:                  apiKindOrNil(item["lastApi"]),
		}
	}
	return models
}

func bucketFrom(value any) *CalibrationBucket {
	record, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	ratio := floatOr(record["ratio"], 0)
	if ratio <= 0 {
		return nil
	}
	samples := numberOr(record["samples"], 0)
	if samples < 0 {
		samples = 0
	}
	return &CalibrationBucket{Ratio: clampRatio(ratio), Samples: samples}
}
