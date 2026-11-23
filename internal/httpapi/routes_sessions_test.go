package httpapi_test

import (
	"net/http"
	"testing"
	"time"
)

func TestSessionDetailRequiresParams(t *testing.T) {
	h := newHarness(t, nil)
	response := h.do("GET", "/api/session", nil, nil)
	if response.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}

func TestHandoffRequiresParams(t *testing.T) {
	h := newHarness(t, nil)
	response := h.do("POST", "/api/handoff", map[string]any{}, nil)
	if response.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}

func TestMetaIdentifiesInstance(t *testing.T) {
	h := newHarness(t, nil)
	response := h.do("GET", "/api/meta", nil, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	data := decode(t, response)
	if data["app"] != "literouter" || data["version"] == "" {
		t.Fatalf("meta: %+v", data)
	}
	if reasons, ok := data["reasons"].([]any); !ok || len(reasons) != 5 {
		t.Fatalf("reasons: %+v", data["reasons"])
	}
}

func TestShutdownRequiresConfiguredToken(t *testing.T) {
	h := newHarness(t, nil)
	response := h.do("POST", "/api/shutdown", map[string]any{}, nil)
	if response.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

func TestShutdownChecksTokenAndStops(t *testing.T) {
	h := newHarness(t, nil)
	stopped := make(chan struct{}, 1)
	h.api.Token = "stop-token"
	h.api.Stop = func() { stopped <- struct{}{} }

	denied := h.do("POST", "/api/shutdown", map[string]any{}, map[string]string{"x-literouter-token": "wrong"})
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("denied status = %d", denied.StatusCode)
	}
	accepted := h.do("POST", "/api/shutdown", map[string]any{}, map[string]string{"x-literouter-token": "stop-token"})
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("accepted status = %d", accepted.StatusCode)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop callback was not invoked")
	}
}
