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

func testServer(c Fetcher, now func() time.Time) http.Handler {
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Car Loan", "9000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(100, "Acme Motor Credit", "450.00", "month", 1, ptr(int64(1)), nil),
		},
	}
	h := testServer(c, fixedNow(testNow))

	for _, path := range []string{"/loans"} {
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Mystery", "500.0000", "active"),
		},
	}
	h := testServer(c, fixedNow(testNow))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/loans", nil))

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
	c := &fakeClient{manualErr: errors.New("401 Unauthorized")}
	h := testServer(c, fixedNow(testNow))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/loans", nil))

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
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/loans?include_credit=yes-please", nil))

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
		{"/loans", 0},
		{"/loans?include_credit=true", 1},
		{"/loans?include_credit=1", 1},
		{"/loans?include_credit=false", 0},
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
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/loans", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
	}

	if c.manualCalls != 1 {
		t.Errorf("manual calls = %d, want 1 (cached)", c.manualCalls)
	}
}

func TestCacheExpires(t *testing.T) {
	c := &countingClient{}
	now := testNow
	s := &Server{Log: zap.NewNop().Sugar(), Client: c, Now: func() time.Time { return now }}
	h := router(s, http.NotFoundHandler())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/loans", nil))
	now = now.Add(cacheTTL + time.Second)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/loans", nil))

	if c.manualCalls != 2 {
		t.Errorf("manual calls = %d, want 2 after TTL expiry", c.manualCalls)
	}
}

func TestCacheIsKeyedByOptions(t *testing.T) {
	c := &countingClient{}
	h := testServer(c, fixedNow(testNow))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/loans", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/loans?include_credit=true", nil))

	if c.manualCalls != 2 {
		t.Errorf("manual calls = %d, want 2; different options must not share a cache entry", c.manualCalls)
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
	manualCalls int
}

func (c *countingClient) GetManualAccounts(_ context.Context) ([]*lunchmoney.ManualAccount, error) {
	c.manualCalls++
	return []*lunchmoney.ManualAccount{
		manualAccount(1, "loan", "Loan", "1000.0000", "active"),
	}, nil
}

func (c *countingClient) GetPlaidAccounts(_ context.Context) ([]*lunchmoney.PlaidAccount, error) {
	return nil, nil
}

func (c *countingClient) GetBudgetSummary(_ context.Context, _ *lunchmoney.BudgetFilters) (*lunchmoney.BudgetSummary, error) {
	return nil, nil
}

func (c *countingClient) GetCategories(_ context.Context, _ *lunchmoney.CategoryFilters) ([]*lunchmoney.Category, error) {
	return nil, nil
}

func (c *countingClient) GetRecurringItems(_ context.Context, _ *lunchmoney.RecurringItemFilters) ([]*lunchmoney.RecurringItem, error) {
	return nil, nil
}

func TestRunCLI(t *testing.T) {
	c := testBudgetFixture(
		[]*lunchmoney.Category{
			categoryRow(1, "Income", asIncome),
			categoryRow(2, "Mortgage"),
		},
		[]*lunchmoney.SummaryCategory{
			summaryRow(1, 5000, -5000),
			summaryRow(2, 2000, 2000),
		},
	)
	c.manualAccounts = []*lunchmoney.ManualAccount{
		manualAccount(1, "loan", "Car Loan", "5000.0000", "active"),
	}

	// CLI text mode
	code := runCLI(context.Background(), c, nil, "2026-08", false)
	if code != 0 {
		t.Errorf("runCLI text exit code = %d, want 0", code)
	}

	// CLI json mode
	code = runCLI(context.Background(), c, nil, "2026-08", true)
	if code != 0 {
		t.Errorf("runCLI json exit code = %d, want 0", code)
	}

	// Invalid month
	code = runCLI(context.Background(), c, nil, "invalid-month", false)
	if code != 1 {
		t.Errorf("runCLI invalid month exit code = %d, want 1", code)
	}
}
