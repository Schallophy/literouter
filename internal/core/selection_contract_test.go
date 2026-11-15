package core

import (
	"testing"
	"time"

	"github.com/Schallophy/literouter/internal/model"
)

func contractRecord(code string, patch func(*model.ModelRecord)) *model.ModelRecord {
	record := &model.ModelRecord{Code: code, Family: "Test", Health: model.HealthAlive}
	if patch != nil {
		patch(record)
	}
	return record
}

func contractPick(t *testing.T, models []string, records map[string]*model.ModelRecord, now int64, retryMs int64, accepts func(string) bool) []string {
	t.Helper()
	candidates := SelectCandidates(SelectionInput{
		Chain:            model.ChainConfig{Models: models},
		GetRecord:        func(code string) *model.ModelRecord { return records[code] },
		Now:              now,
		RetryDeadAfterMs: retryMs,
		Accepts:          accepts,
	})
	codes := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		codes = append(codes, candidate.Code)
	}
	return codes
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func TestSelectionContract(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC).UnixMilli()
	iso := func(ms int64) *string {
		text := time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
		return &text
	}

	t.Run("auto resolves to the first model in the list", func(t *testing.T) {
		resolution := ResolveRoute("auto", model.ChainConfig{Models: []string{"a", "b", "c"}}, nil)
		if resolution.Mode != RouteAuto || resolution.Model != "a" {
			t.Fatalf("got %+v", resolution)
		}
	})
	t.Run("an empty model name is treated as auto", func(t *testing.T) {
		resolution := ResolveRoute("", model.ChainConfig{Models: []string{"a", "b"}}, nil)
		if resolution.Mode != RouteAuto || resolution.Model != "a" {
			t.Fatalf("got %+v", resolution)
		}
	})
	t.Run("auto resolves to null when the list is empty", func(t *testing.T) {
		if resolution := ResolveRoute("auto", model.ChainConfig{}, nil); resolution.Model != "" {
			t.Fatalf("got %+v", resolution)
		}
	})
	t.Run("a model in the list is resolved as a direct target", func(t *testing.T) {
		resolution := ResolveRoute("c", model.ChainConfig{Models: []string{"a", "b", "c"}}, nil)
		if resolution.Mode != RouteDirect || resolution.Model != "c" {
			t.Fatalf("got %+v", resolution)
		}
	})
	t.Run("unknown models pass through untouched", func(t *testing.T) {
		resolution := ResolveRoute("some-external-model", model.ChainConfig{Models: []string{"a", "b"}}, nil)
		if resolution.Mode != RoutePassthrough || resolution.Model != "some-external-model" {
			t.Fatalf("got %+v", resolution)
		}
	})
	t.Run("dead and invalid models are excluded", func(t *testing.T) {
		records := map[string]*model.ModelRecord{
			"a": contractRecord("a", func(r *model.ModelRecord) { r.Health = model.HealthDead }),
			"b": contractRecord("b", nil),
			"c": contractRecord("c", func(r *model.ModelRecord) { r.Health = model.HealthInvalid }),
		}
		if got := contractPick(t, []string{"a", "b", "c"}, records, now, 0, nil); !equalStrings(got, []string{"b"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("a dead model returns once its retry interval has elapsed", func(t *testing.T) {
		records := map[string]*model.ModelRecord{
			"a": contractRecord("a", func(r *model.ModelRecord) {
				r.Health = model.HealthDead
				r.CheckedAt = iso(now - 23*3_600_000)
			}),
			"b": contractRecord("b", nil),
		}
		if got := contractPick(t, []string{"a", "b"}, records, now, 24*3_600_000, nil); !equalStrings(got, []string{"b"}) {
			t.Fatalf("24h: %v", got)
		}
		if got := contractPick(t, []string{"a", "b"}, records, now, 12*3_600_000, nil); !equalStrings(got, []string{"a", "b"}) {
			t.Fatalf("12h: %v", got)
		}
		if got := contractPick(t, []string{"a", "b"}, records, now, 0, nil); !equalStrings(got, []string{"b"}) {
			t.Fatalf("disabled: %v", got)
		}
	})
	t.Run("invalid models never come back automatically", func(t *testing.T) {
		records := map[string]*model.ModelRecord{
			"a": contractRecord("a", func(r *model.ModelRecord) {
				r.Health = model.HealthInvalid
				r.CheckedAt = iso(now - 365*24*3_600_000)
			}),
			"b": contractRecord("b", nil),
		}
		if got := contractPick(t, []string{"a", "b"}, records, now, 60_000, nil); !equalStrings(got, []string{"b"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("throttled models leave the chain until the cooldown ends", func(t *testing.T) {
		records := map[string]*model.ModelRecord{
			"a": contractRecord("a", func(r *model.ModelRecord) { r.ThrottledUntil = iso(now + 60_000) }),
			"b": contractRecord("b", nil),
			"c": contractRecord("c", nil),
		}
		if got := contractPick(t, []string{"a", "b", "c"}, records, now, 0, nil); !equalStrings(got, []string{"b", "c"}) {
			t.Fatalf("cooling: %v", got)
		}
		if got := contractPick(t, []string{"a", "b", "c"}, records, now+60_001, 0, nil); !equalStrings(got, []string{"a", "b", "c"}) {
			t.Fatalf("expired: %v", got)
		}
	})
	t.Run("candidates keep the list order", func(t *testing.T) {
		records := map[string]*model.ModelRecord{"a": contractRecord("a", nil), "b": contractRecord("b", nil), "c": contractRecord("c", nil)}
		if got := contractPick(t, []string{"c", "a", "b"}, records, now, 0, nil); !equalStrings(got, []string{"c", "a", "b"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("a dead first model yields to the next one", func(t *testing.T) {
		records := map[string]*model.ModelRecord{
			"a": contractRecord("a", func(r *model.ModelRecord) { r.Health = model.HealthDead }),
			"b": contractRecord("b", nil),
			"c": contractRecord("c", nil),
		}
		if got := contractPick(t, []string{"a", "b", "c"}, records, now, 0, nil); !equalStrings(got, []string{"b", "c"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("an empty list yields no candidates", func(t *testing.T) {
		if got := contractPick(t, []string{}, map[string]*model.ModelRecord{}, now, 0, nil); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("an accepts filter excludes models without the requested API", func(t *testing.T) {
		records := map[string]*model.ModelRecord{"a": contractRecord("a", nil), "b": contractRecord("b", nil), "c": contractRecord("c", nil)}
		got := contractPick(t, []string{"a", "b", "c"}, records, now, 0, func(code string) bool { return code == "b" })
		if !equalStrings(got, []string{"b"}) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("accepts runs before health so a dead unsupported model is simply absent", func(t *testing.T) {
		records := map[string]*model.ModelRecord{
			"a": contractRecord("a", func(r *model.ModelRecord) { r.Health = model.HealthDead }),
			"b": contractRecord("b", nil),
		}
		got := contractPick(t, []string{"a", "b"}, records, now, 0, func(code string) bool { return code == "a" })
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
}
