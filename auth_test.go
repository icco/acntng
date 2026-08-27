package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icco/lunchmoney"
	"go.uber.org/zap"
)

func keyedServer(key string) http.Handler {
	s := &Server{
		Log:       zap.NewNop().Sugar(),
		SharedKey: key,
		Now:       fixedNow(testNow),
		Client: &fakeClient{
			assets: []*lunchmoney.Asset{
				{ID: 1, TypeName: "loan", Name: "Loan", Balance: "1000.0000", Currency: "usd", Status: "active"},
			},
		},
	}
	// A stand-in for the prometheus handler.
	return router(s, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestSharedKeyRequiredWhenSet(t *testing.T) {
	h := keyedServer("s3cret")

	tests := []struct {
		name   string
		header string
		set    bool
		want   int
	}{
		{"no header", "", false, http.StatusForbidden},
		{"empty header", "", true, http.StatusForbidden},
		{"wrong key", "nope", true, http.StatusForbidden},
		{"prefix of key", "s3cre", true, http.StatusForbidden},
		{"key plus suffix", "s3cretx", true, http.StatusForbidden},
		{"correct key", "s3cret", true, http.StatusOK},
	}

	for _, tt := range tests {
		for _, path := range []string{"/", "/loans"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if tt.set {
				req.Header.Set(sharedKeyHeader, tt.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != tt.want {
				t.Errorf("%s %s: status = %d, want %d", tt.name, path, w.Code, tt.want)
			}
			if tt.want == http.StatusForbidden && strings.Contains(w.Body.String(), "balance") {
				t.Errorf("%s %s: leaked loan data: %s", tt.name, path, w.Body.String())
			}
		}
	}
}

func TestSharedKeyDisabledWhenEmpty(t *testing.T) {
	// Local development runs without a key.
	h := keyedServer("")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 when no key is configured", w.Code)
	}
}

func TestHealthAndMetricsBypassSharedKey(t *testing.T) {
	// The container healthcheck and metrics scraper cannot supply the key,
	// and neither endpoint exposes account data.
	h := keyedServer("s3cret")

	for _, path := range []string{"/healthz", "/metrics"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))

		if w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, w.Code)
		}
	}
}

func TestForgedPortalHeadersDoNotGrantAccess(t *testing.T) {
	// A sibling container on the shared docker network can forge whatever the
	// auth portal injects, so those headers must not be treated as proof.
	h := keyedServer("s3cret")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-WEBAUTH-USER", "github.com/icco")
	req.Header.Set("X-WEBAUTH-EMAIL", "nat@natwelch.com")
	req.Header.Set("Authorization", "Bearer anything")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; portal headers must not authenticate", w.Code)
	}
}
