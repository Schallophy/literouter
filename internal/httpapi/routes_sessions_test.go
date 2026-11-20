package httpapi_test

import "testing"

func TestSessionsHandoffRequiresParams(t *testing.T) {
	h := newHarness(t, nil)

	response := h.do("GET", "/api/sessions/handoff", nil, nil)
	if response.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}

func TestSessionsDetailRequiresParams(t *testing.T) {
	h := newHarness(t, nil)

	response := h.do("GET", "/api/sessions/detail", nil, nil)
	if response.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}
