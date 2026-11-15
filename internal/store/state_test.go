package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Schallophy/literouter/internal/model"
	"github.com/Schallophy/literouter/internal/util"
)

func openTemp(t *testing.T, codes []string) (*StateStore, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "state.json")
	store, err := Open(OpenOptions{File: file, Codes: codes})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return store, file
}

func TestEnsureAndHealthRoundTrip(t *testing.T) {
	store, file := openTemp(t, []string{"model-a"})
	store.SetHealth("model-a", model.HealthDead, map[string]any{
		"reason":       "额度已耗尽",
		"lastStatusCode": 403.0,
		"checkedAt":    "2026-01-01T00:00:00.000Z",
	})
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	reopened, err := Open(OpenOptions{File: file, Codes: nil})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	record := reopened.Get("model-a")
	if record == nil || record.Health != model.HealthDead || record.Reason == nil || *record.Reason != "额度已耗尽" {
		t.Fatalf("got %+v", record)
	}
	if record.LastStatusCode == nil || *record.LastStatusCode != 403 {
		t.Fatalf("status code lost: %+v", record.LastStatusCode)
	}
	if record.Family != "Other" {
		t.Fatalf("family: %s", record.Family)
	}
}

func TestCallerUsagePersistsAcrossReopen(t *testing.T) {
	store, file := openTemp(t, nil)
	caller := util.IdentifyCallerValue("Bearer sk-opencode")
	store.RecordCallerRequest(caller, true, 1, stringPtr("model-a"), model.ApiChat)
	store.AddCallerUsage(caller, &model.Usage{
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CachedPromptTokens: 8, CacheReported: true,
	}, "model-a", 0, model.ApiChat, nil)
	store.RecordCallerRequest(caller, false, 0, nil, model.ApiChat)
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	reopened, err := Open(OpenOptions{File: file})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	callers := reopened.Callers()
	if len(callers) != 1 {
		t.Fatalf("callers %d", len(callers))
	}
	record := callers[0]
	if record.Label != "opencode" || record.Requests != 2 || record.Failures != 1 || record.Failovers != 1 {
		t.Fatalf("got %+v", record)
	}
	if record.PromptTokens != 10 || record.CachedPromptTokens != 8 || record.CacheTrackedPromptTokens != 10 || record.CompletionTokens != 5 {
		t.Fatalf("tokens %+v", record)
	}
	if record.LastModel == nil || *record.LastModel != "model-a" {
		t.Fatalf("last model %+v", record.LastModel)
	}
}

func TestCacheUsageAndRecomputeCosts(t *testing.T) {
	store, file := openTemp(t, []string{"model-a"})
	caller := util.IdentifyCallerValue("Bearer sk-opencode")
	usage := model.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CachedPromptTokens: 80, CacheReported: true}
	store.MarkUsed("model-a", &usage, 0.5, model.ApiChat, nil)
	store.AddCallerUsage(caller, &usage, "model-a", 0.5, model.ApiChat, nil)
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	reopened, err := Open(OpenOptions{File: file, Codes: []string{"model-a"}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	record := reopened.Get("model-a")
	if record.PromptTokens != 100 || record.CachedPromptTokens != 80 || record.CacheTrackedPromptTokens != 100 || record.Cost != 0.5 {
		t.Fatalf("model %+v", record)
	}
	stats := reopened.Stats()
	if stats.PromptTokens != 100 || stats.CachedPromptTokens != 80 || stats.CacheTrackedPromptTokens != 100 || stats.Cost != 0.5 {
		t.Fatalf("stats %+v", stats)
	}
	callers := reopened.Callers()
	if callers[0].CachedPromptTokens != 80 || callers[0].CacheTrackedPromptTokens != 100 || callers[0].Cost != 0.5 {
		t.Fatalf("caller %+v", callers[0])
	}

	reopened.RecomputeCosts(func(modelCode string, usage UsageTokens) model.CostSplit {
		if modelCode == "model-a" {
			return model.CostSplit{Input: 2}
		}
		return model.CostSplit{}
	})
	if reopened.Stats().Cost != 2 || reopened.Get("model-a").Cost != 2 || reopened.Callers()[0].Cost != 2 {
		t.Fatalf("recompute failed: %+v %+v", reopened.Stats(), reopened.Callers())
	}
}

func TestCallerPerModelTotals(t *testing.T) {
	store, file := openTemp(t, []string{"model-a", "model-b"})
	caller := util.IdentifyCallerValue("Bearer sk-opencode")
	usageA := model.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, CachedPromptTokens: 80, CacheReported: true}
	usageB := model.Usage{PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55, CachedPromptTokens: 0, CacheReported: false}
	partsA := model.CostSplit{Input: 0.2, CachedInput: 0.1, Output: 0.3, CacheSaved: 0.4}
	partsB := model.CostSplit{Input: 0.15, CachedInput: 0, Output: 0.05, CacheSaved: 0}

	store.AddCallerUsage(caller, &usageA, "model-a", 0.6, model.ApiChat, &partsA)
	store.AddCallerUsage(caller, &usageB, "model-b", 0.2, model.ApiChat, &partsB)
	store.MarkCallerModelFailure(caller, "model-a", model.ApiResponses)
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	reopened, err := Open(OpenOptions{File: file, Codes: []string{"model-a", "model-b"}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	record := reopened.Callers()[0]
	if record.PromptTokens != 150 || record.CachedPromptTokens != 80 || record.CacheTrackedPromptTokens != 100 || record.CompletionTokens != 15 {
		t.Fatalf("caller tokens %+v", record)
	}
	if record.Cost != 0.8 || record.CostBreakdown.Input != 0.35 || record.CostBreakdown.CachedInput != 0.1 ||
		record.CostBreakdown.Output != 0.35 || record.CostBreakdown.CacheSaved != 0.4 || record.CostBreakdown.Estimated {
		t.Fatalf("caller split %+v", record.CostBreakdown)
	}
	a := record.Models["model-a"]
	if a == nil || a.Attempts != 2 || a.Requests != 1 || a.Failures != 1 || a.PromptTokens != 100 ||
		a.CachedPromptTokens != 80 || a.CompletionTokens != 10 || a.Cost != 0.6 ||
		a.CostBreakdown.Input != 0.2 || a.CostBreakdown.CacheSaved != 0.4 || a.LastAPI == nil || *a.LastAPI != model.ApiResponses {
		t.Fatalf("model-a %+v", a)
	}
	b := record.Models["model-b"]
	if b == nil || b.Attempts != 1 || b.Requests != 1 || b.Failures != 0 || b.PromptTokens != 50 ||
		b.CacheTrackedPromptTokens != 0 || b.Cost != 0.2 || b.LastAPI == nil || *b.LastAPI != model.ApiChat {
		t.Fatalf("model-b %+v", b)
	}
}

func TestHydratesHistoricalState(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "state.json"))
	if err != nil {
		t.Skipf("historical state fixture missing: %v", err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "state.json")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := Open(OpenOptions{File: file})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	record := store.Get("qianwenai/deepseek-v4.1-flash-1017")
	if record == nil {
		t.Fatal("historical model missing")
	}
	if record.PromptTokens != 445617557 || record.CompletionTokens != 1838170 || record.Requests != 3163 || record.Failures != 23 {
		t.Fatalf("historical counters lost: %+v", record)
	}
	if record.Cost < 105.08 || record.Cost > 105.09 || !record.CostBreakdown.Estimated || record.CostBreakdown.CacheSaved < 684 {
		t.Fatalf("historical cost lost: %+v", record)
	}
	if len(store.All()) == 0 {
		t.Fatal("models empty")
	}
	stats := store.Stats()
	if stats.Requests == 0 || stats.PromptTokens == 0 || len(store.Callers()) == 0 {
		t.Fatalf("stats/callers empty: %+v", stats)
	}
	// A flush must keep counters intact and write valid JSON.
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if decoded["version"] != 2.0 {
		t.Fatalf("version %v", decoded["version"])
	}
}

func TestCorruptStateRecovered(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "state.json")
	if err := os.WriteFile(file, []byte("{ nope"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	called := false
	store, err := Open(OpenOptions{File: file, OnError: func(error) { called = true }})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !called {
		t.Fatal("onError not called")
	}
	if store.Get("anything") != nil {
		t.Fatal("expected fresh state")
	}
	matches, _ := filepath.Glob(file + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("backup missing: %v", matches)
	}
}

func TestMigrateModelCodesMerges(t *testing.T) {
	store, _ := openTemp(t, []string{"plain-a", "provider/a"})
	usage := model.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}
	store.MarkUsed("plain-a", &usage, 0.1, model.ApiChat, nil)
	store.MarkUsed("provider/a", &usage, 0.2, model.ApiChat, nil)
	store.ObserveCalibration("plain-a", false, 1000, 1500)
	store.MigrateModelCodes(func(code string) string {
		if code == "plain-a" {
			return "provider/a"
		}
		return code
	})
	record := store.Get("provider/a")
	if record == nil || record.PromptTokens != 20 || record.CompletionTokens != 4 || record.Cost < 0.29 {
		t.Fatalf("merge failed: %+v", record)
	}
	if store.Get("plain-a") != nil {
		t.Fatal("legacy record must be removed")
	}
	if store.CalibrationRatio("provider/a", false) <= 1 {
		t.Fatalf("calibration lost: %v", store.CalibrationRatio("provider/a", false))
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func TestBackfillUsageDetailsSoleCaller(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "state.json")
	legacy := map[string]any{
		"version": 1,
		"models": map[string]any{
			"model-a": map[string]any{
				"code": "model-a", "health": "alive", "promptTokens": 1000, "completionTokens": 100,
				"cost": 0.6, "requests": 3, "failures": 0,
			},
		},
		"callers": map[string]any{
			"caller-1": map[string]any{
				"label": "opencode", "requests": 3, "promptTokens": 1000, "completionTokens": 100, "cost": 0.6,
			},
		},
		"stats": map[string]any{
			"requests": 3, "promptTokens": 1000, "completionTokens": 100, "cost": 0.6,
			"apis": map[string]any{"chat": map[string]any{"requests": 3, "promptTokens": 1000, "completionTokens": 100, "cost": 0.6, "lastModel": "model-a"}},
		},
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(file, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := Open(OpenOptions{File: file, Codes: []string{"model-a"}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	changed := store.BackfillUsageDetails(func(modelCode string, usage UsageTokens) *model.CostSplit {
		split := model.CostSplit{Input: 0.4, CachedInput: 0.1, Output: 0.1, CacheSaved: 0}
		return &split
	})
	if !changed {
		t.Fatal("expected backfill")
	}
	record := store.Get("model-a")
	if record.CostBreakdown.Total() == 0 || !record.CostBreakdown.Estimated {
		t.Fatalf("model breakdown not filled: %+v", record.CostBreakdown)
	}
	callers := store.Callers()
	if len(callers) != 1 {
		t.Fatalf("callers %d", len(callers))
	}
	if _, ok := callers[0].Models["model-a"]; !ok {
		t.Fatalf("caller models not seeded: %+v", callers[0].Models)
	}
	if mathAbs(store.Stats().CostBreakdown.Total()-store.Stats().Cost) > 1e-6 {
		t.Fatalf("stats breakdown %+v", store.Stats().CostBreakdown)
	}
}

func mathAbs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func TestResetUsagePreservesHealth(t *testing.T) {
	store, _ := openTemp(t, []string{"model-a"})
	usage := model.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}
	store.MarkUsed("model-a", &usage, 0.1, model.ApiChat, nil)
	store.SetHealth("model-a", model.HealthDead, map[string]any{"reason": "x"})
	store.ResetUsage()
	record := store.Get("model-a")
	if record.Requests != 0 || record.PromptTokens != 0 || record.Cost != 0 || record.Health != model.HealthDead {
		t.Fatalf("reset: %+v", record)
	}
	if len(store.Callers()) != 0 || store.Stats().Requests != 0 {
		t.Fatalf("callers/stats not reset")
	}
}

func TestConcurrentUsageUpdates(t *testing.T) {
	store, _ := openTemp(t, []string{"model-a"})
	caller := util.IdentifyCallerValue("Bearer sk-opencode")
	var wg sync.WaitGroup
	for index := 0; index < 50; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			usage := model.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, CacheReported: true}
			store.MarkUsed("model-a", &usage, 0.001, model.ApiChat, nil)
			store.AddCallerUsage(caller, &usage, "model-a", 0.001, model.ApiChat, nil)
			_ = store.Get("model-a")
			_ = store.Stats()
		}()
	}
	wg.Wait()
	record := store.Get("model-a")
	if record.Requests != 50 || record.PromptTokens != 50 || record.CompletionTokens != 50 {
		t.Fatalf("lost updates: %+v", record)
	}
}

func TestSetCatalogAndPrune(t *testing.T) {
	store, _ := openTemp(t, []string{"keep/a", "drop/b"})
	store.SetCatalog([]struct{ ID, Provider string }{
		{ID: "qwen", Provider: "keep"},
		{ID: "qwen", Provider: "keep"},
		{ID: "kimi", Provider: "other"},
	})
	_, catalog := store.Catalog()
	if len(catalog) != 2 || catalog[0].Family == "" {
		t.Fatalf("catalog %+v", catalog)
	}
	store.PruneModels([]string{"keep/a"})
	if store.Get("drop/b") != nil {
		t.Fatal("prune failed")
	}
}

func TestAtomicWriteLeavesNoTemp(t *testing.T) {
	store, file := openTemp(t, []string{"a"})
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	matches, _ := filepath.Glob(file + ".*.tmp")
	if len(matches) != 0 {
		t.Fatalf("temp files left: %v", matches)
	}
}
