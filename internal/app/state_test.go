package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	t.Setenv("LITEROUTER_STATE", filepath.Join(t.TempDir(), "server.json"))
	if _, ok := readState(); ok {
		t.Fatal("unexpected state before writing")
	}
	want := serverState{
		Address: "http://127.0.0.1:8788", Port: 8788, PID: 42, Version: "test", Token: "tok",
	}
	if err := writeState(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := readState()
	if !ok || got != want {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	clearState()
	if _, ok := readState(); ok {
		t.Fatal("state not cleared")
	}
}

func TestLiveInstanceProbesMeta(t *testing.T) {
	t.Setenv("LITEROUTER_STATE", filepath.Join(t.TempDir(), "server.json"))

	same := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/meta" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "literouter", "version": "test"})
	}))
	defer same.Close()
	if err := writeState(serverState{Address: same.URL, Port: 1234, PID: 1, Version: "test"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, ok := liveInstance(); !ok || got.Address != same.URL {
		t.Fatalf("live instance: %+v ok=%v", got, ok)
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"app": "something-else", "version": "test"})
	}))
	defer other.Close()
	if err := writeState(serverState{Address: other.URL, Port: 1235, PID: 2, Version: "test"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := liveInstance(); ok {
		t.Fatal("foreign service must not count as a live instance")
	}
	if _, err := os.Stat(statePath()); !os.IsNotExist(err) {
		t.Fatalf("stale state should be removed, stat err=%v", err)
	}
}

func TestStopInstanceSendsToken(t *testing.T) {
	t.Setenv("LITEROUTER_STATE", filepath.Join(t.TempDir(), "server.json"))
	token := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/shutdown" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		token <- r.Header.Get("x-literouter-token")
		_ = json.NewEncoder(w).Encode(map[string]any{"stopping": true})
	}))
	defer server.Close()

	if err := stopInstance(serverState{Address: server.URL, Token: "secret", PID: 999999}, time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case got := <-token:
		if got != "secret" {
			t.Fatalf("token = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown endpoint was not called")
	}
}
