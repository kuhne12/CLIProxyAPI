package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBPSImageRelayRoutesPreserveSignedRequest(t *testing.T) {
	const target = "/api/bps-images/signed-token"
	calls := 0
	s := newTestServerWithOptions(t, WithBPSImageRelayHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != target {
			t.Errorf("relay path changed: %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		s.engine.ServeHTTP(w, httptest.NewRequest(method, target, nil))
		if w.Code != http.StatusNoContent {
			t.Fatalf("signed download requires unexpected authentication: %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, target, nil))
	if calls != 2 {
		t.Fatal("download handler accepted unsupported method")
	}
}
