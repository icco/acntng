package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/icco/lunchmoney"
	"go.uber.org/zap"
)

func testServer(c LoanFetcher, now func() time.Time) http.Handler {
	s := &Server{
		Log:    zap.NewNop().Sugar(),
		Client: c,
		Now:    now,
	}
	return router(s, http.NotFoundHandler())
}

func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestHandleLoansReturnsJSON(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Car Loan", Balance: "9000.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringExpense{
			{ID: 100, AssetID: 1, Payee: "Acme Motor Credit", Amount: "450.00", Currency: "usd", Cadence: "monthly"},
		},
	}
	h := testServer(c, fixedNow(testNow))

	for _, path := range []string{"/", "/loans"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))

		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body %s", path, w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s content-type = %q, want json", path, ct)
		}

		var rep Report
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatalf("%s: could not decode body %s: %v", path, w.Body.String(), err)
		}
		if rep.Totals.Count != 1 || rep.Totals.MonthlyPayment != 450 {
			t.Errorf("%s totals = %+v", path, rep.Totals)
		}
	}
}

func TestMonthlyPaymentSerializesAsNull(t *testing.T) {
	// A loan with no derivable payment must be null, not 0 -- a consumer has
	// to be able to tell "unknown" from "nothing owed monthly".
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Mystery", Balance: "500.0000", Currency: "usd", Status: "active"},
		},
	}
	h := testServer(c, fixedNow(testNow))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

	body := w.Body.String()
	if !strings.Contains(body, `"monthly_payment": null`) && !strings.Contains(body, `"monthly_payment":null`) {
		t.Errorf("want a null monthly_payment in %s", body)
	}
	if !strings.Contains(body, `"payment_source": "none"`) && !strings.Contains(body, `"payment_source":"none"`) {
		t.Errorf("want payment_source none in %s", body)
	}
}

func TestHealthz(t *testing.T) {
	h := testServer(&fakeClient{}, fixedNow(testNow))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestUpstreamFailureIsBadGateway(t *testing.T) {
	c := &fakeClient{assetsErr: errors.New("401 Unauthorized")}
	h := testServer(c, fixedNow(testNow))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	// The upstream error may name the token; it must not reach the client.
	if strings.Contains(w.Body.String(), "Unauthorized") {
		t.Errorf("upstream error leaked to the client: %s", w.Body.String())
	}
}

func TestBadQueryParamIsBadRequest(t *testing.T) {
	h := testServer(&fakeClient{}, fixedNow(testNow))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/?include_credit=yes-please", nil))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
}

func TestIncludeCreditQueryParam(t *testing.T) {
	c := &fakeClient{
		plaid: []*lunchmoney.PlaidAccount{
			{ID: 11, Type: "credit", Name: "Amex", Balance: "1200.0000", Currency: "usd", Status: "active"},
		},
	}
	h := testServer(c, fixedNow(testNow))

	for _, tt := range []struct {
		query string
		want  int
	}{
		{"/", 0},
		{"/?include_credit=true", 1},
		{"/?include_credit=1", 1},
		{"/?include_credit=false", 0},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.query, nil))

		var rep Report
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatalf("%s: %v", tt.query, err)
		}
		if rep.Totals.Count != tt.want {
			t.Errorf("%s count = %d, want %d", tt.query, rep.Totals.Count, tt.want)
		}
	}
}

func TestCacheAvoidsRefetch(t *testing.T) {
	c := &countingClient{}
	h := testServer(c, fixedNow(testNow))

	for range 3 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
	}

	if c.assetCalls != 1 {
		t.Errorf("asset calls = %d, want 1 (cached)", c.assetCalls)
	}
}

func TestCacheExpires(t *testing.T) {
	c := &countingClient{}
	now := testNow
	s := &Server{Log: zap.NewNop().Sugar(), Client: c, Now: func() time.Time { return now }}
	h := router(s, http.NotFoundHandler())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	now = now.Add(cacheTTL + time.Second)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

	if c.assetCalls != 2 {
		t.Errorf("asset calls = %d, want 2 after TTL expiry", c.assetCalls)
	}
}

func TestCacheIsKeyedByOptions(t *testing.T) {
	c := &countingClient{}
	h := testServer(c, fixedNow(testNow))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/?include_credit=true", nil))

	if c.assetCalls != 2 {
		t.Errorf("asset calls = %d, want 2; different options must not share a cache entry", c.assetCalls)
	}
}

func TestParseOverrides(t *testing.T) {
	got, err := parseOverrides(`{"asset:1": 450.5, "plaid:10": 1200}`)
	if err != nil {
		t.Fatalf("parseOverrides: %v", err)
	}
	if got["asset:1"] != 450.5 || got["plaid:10"] != 1200 {
		t.Errorf("overrides = %v", got)
	}

	if got, err := parseOverrides(""); err != nil || got != nil {
		t.Errorf("empty = (%v, %v), want (nil, nil)", got, err)
	}

	if _, err := parseOverrides("not json"); err == nil {
		t.Error("want an error for malformed overrides")
	}
}

// countingClient records how many times each endpoint was hit.
type countingClient struct {
	assetCalls int
}

func (c *countingClient) GetAssets(_ context.Context) ([]*lunchmoney.Asset, error) {
	c.assetCalls++
	return []*lunchmoney.Asset{
		{ID: 1, TypeName: "loan", Name: "Loan", Balance: "1000.0000", Currency: "usd", Status: "active"},
	}, nil
}

func (c *countingClient) GetPlaidAccounts(_ context.Context) ([]*lunchmoney.PlaidAccount, error) {
	return nil, nil
}

func (c *countingClient) GetRecurringExpenses(_ context.Context, _ *lunchmoney.RecurringExpenseFilters) ([]*lunchmoney.RecurringExpense, error) {
	return nil, nil
}
