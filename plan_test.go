package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.icco.me/lunchmoney"
	"go.uber.org/zap"
)

// GetTransactions serves the fixture a page at a time, so pagination is
// exercised whenever pageSize is set.
func (f *fakeClient) GetTransactions(_ context.Context, filters *lunchmoney.TransactionFilters) (*lunchmoney.TransactionsResponse, error) {
	f.transactionGets++
	if f.transactionErr != nil {
		return nil, f.transactionErr
	}

	var in []*lunchmoney.Transaction
	for _, tx := range f.transactions {
		if tx.Date >= *filters.StartDate && tx.Date <= *filters.EndDate {
			in = append(in, tx)
		}
	}

	offset := int(*filters.Offset)
	if offset >= len(in) {
		return &lunchmoney.TransactionsResponse{}, nil
	}
	size := f.pageSize
	if size == 0 {
		size = len(in)
	}
	end := min(offset+size, len(in))
	return &lunchmoney.TransactionsResponse{Transactions: in[offset:end], HasMore: end < len(in)}, nil
}

const (
	catIncome int64 = iota + 1
	catGroceries
	catRestaurants
	catMortgage
	catFees
	catTransfer
	catSubscriptions
)

func planCategories() []*lunchmoney.Category {
	return []*lunchmoney.Category{
		{ID: catIncome, Name: "Income", IsIncome: true},
		{ID: catGroceries, Name: "Groceries"},
		{ID: catRestaurants, Name: "Restaurants"},
		{ID: catMortgage, Name: "Mortgage"},
		{ID: catFees, Name: "Fees"},
		{ID: catTransfer, Name: "Payment, Transfer", ExcludeFromBudget: true, ExcludeFromTotals: true},
		{ID: catSubscriptions, Name: "Subscriptions"},
	}
}

var txID int64

// tx builds a transaction. A positive amount is spending, as in Lunch Money.
func tx(date, payee string, amount float64, category int64) *lunchmoney.Transaction {
	txID++
	t := &lunchmoney.Transaction{ID: txID, Date: date, Payee: payee, OriginalName: payee, ToBase: amount}
	if category != 0 {
		t.CategoryID = &category
	}
	return t
}

// planNow is late November, so October is the last complete month.
var planNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// planFixture is six baseline months (April to September) and an October
// that overspent on restaurants.
func planFixture() *fakeClient {
	var txs []*lunchmoney.Transaction
	restaurants := map[string]float64{
		"2026-04": 300, "2026-05": 400, "2026-06": 500, "2026-07": 600, "2026-08": 400, "2026-09": 500,
	}
	groceries := map[string]float64{
		"2026-04": 500, "2026-05": 700, "2026-06": 600, "2026-07": 600, "2026-08": 800, "2026-09": 600,
	}
	for _, m := range []string{"2026-04", "2026-05", "2026-06", "2026-07", "2026-08", "2026-09", "2026-10"} {
		txs = append(txs,
			tx(m+"-15", "ACME PAYROLL 260415 8812 ADA LOVELACE", -3000, catIncome),
			tx(m+"-28", "ACME PAYROLL 260428 8813 ADA LOVELACE", -3000, catIncome),
			tx(m+"-30", "Interest Payment", -2, catIncome),
			tx(m+"-01", "Example Bank Mortgage", 2000, catMortgage),
			tx(m+"-02", "Card Payment", 1500, catTransfer),
			tx(m+"-08", "Streamflix", 15, catSubscriptions),
		)
		if m != "2026-10" {
			txs = append(txs,
				tx(m+"-10", "Corner Bistro", restaurants[m], catRestaurants),
				tx(m+"-12", "Fresh Market", groceries[m], catGroceries),
			)
		}
	}
	txs = append(txs,
		tx("2026-10-03", "Noodle Cart", 30, catRestaurants),
		tx("2026-10-09", "Noodle Cart", 30, catRestaurants),
		tx("2026-10-17", "Noodle Cart", 30, catRestaurants),
		tx("2026-10-24", "Noodle Cart", 30, catRestaurants),
		tx("2026-10-11", "Corner Bistro", 680, catRestaurants),
		tx("2026-10-12", "Fresh Market", 600, catGroceries),
		tx("2026-10-20", "Monthly Service Fee", 25, catFees),
		tx("2026-10-21", "Mystery Shop", 400, 0),
		tx("2026-10-22", "Unlabelled Deposit", -90, 0),
	)
	pending := tx("2026-10-23", "Pending Diner", 999, catRestaurants)
	pending.IsPending = true
	txs = append(txs, pending)

	return &fakeClient{categories: planCategories(), transactions: txs}
}

func buildFixturePlan(t *testing.T, c *fakeClient) *Plan {
	t.Helper()
	p, err := BuildPlan(context.Background(), c, PlanOptions{Review: lastCompleteMonth(planNow), Now: planNow})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func line(t *testing.T, p *Plan, name string) CategoryLine {
	t.Helper()
	for _, l := range p.Categories {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("no category %q in %+v", name, p.Categories)
	return CategoryLine{}
}

func suggestion(p *Plan, kind, category string) *Suggestion {
	for i, s := range p.Suggestions {
		if s.Kind == kind && (category == "" || s.Category == category) {
			return &p.Suggestions[i]
		}
	}
	return nil
}

func TestBuildPlanComparesPaychecksToNormalSpend(t *testing.T) {
	p := buildFixturePlan(t, planFixture())

	if p.Month != "2026-10" || p.PlanFor != "2026-12" {
		t.Errorf("month = %s, plan for = %s; want 2026-10 and 2026-12", p.Month, p.PlanFor)
	}
	if len(p.BaselineMonths) != 6 || p.BaselineMonths[0] != "2026-04" {
		t.Errorf("baseline = %v", p.BaselineMonths)
	}

	tot := p.Totals
	if tot.PaycheckNormal != 6000 || tot.PaycheckReview != 6000 {
		t.Errorf("paychecks normal/review = %v/%v, want 6000/6000", tot.PaycheckNormal, tot.PaycheckReview)
	}
	if tot.OtherIncomeNormal != 2 {
		t.Errorf("other income = %v, want 2: interest is income but not a paycheck", tot.OtherIncomeNormal)
	}

	// Medians: mortgage 2000, groceries 600, restaurants 450, subscriptions 15.
	if tot.SpendNormal != 3065 || tot.FixedNormal != 2000 || tot.FlexibleNormal != 1065 {
		t.Errorf("normal spend total/fixed/flexible = %v/%v/%v", tot.SpendNormal, tot.FixedNormal, tot.FlexibleNormal)
	}
	if tot.MarginNormal != 2935 {
		t.Errorf("normal margin = %v, want 2935", tot.MarginNormal)
	}
	// October: 2000 + 600 + 800 + 15 + 25 fees + 400 uncategorized.
	if tot.SpendReview != 3840 || tot.MarginReview != 2160 {
		t.Errorf("review spend/margin = %v/%v, want 3840/2160", tot.SpendReview, tot.MarginReview)
	}
	if tot.SavingsRate == nil || *tot.SavingsRate != 0.49 {
		t.Errorf("savings rate = %v, want 0.49", tot.SavingsRate)
	}

	r := line(t, p, "Restaurants")
	if r.Normal != 450 || r.Low != 400 || r.Review != 800 || r.Delta != 350 || r.Fixed {
		t.Errorf("restaurants = %+v", r)
	}
	if m := line(t, p, "Mortgage"); !m.Fixed || m.Target != 2000 {
		t.Errorf("mortgage = %+v, want fixed with target 2000", m)
	}
	for _, l := range p.Categories {
		if l.Name == "Payment, Transfer" {
			t.Errorf("excluded category is in the plan: %+v", l)
		}
	}
}

func TestBuildPlanSuggestions(t *testing.T) {
	p := buildFixturePlan(t, planFixture())

	over := suggestion(p, KindOverNormal, "Restaurants")
	if over == nil || *over.MonthlySavings != 350 {
		t.Fatalf("want an over_normal restaurants suggestion saving 350, got %+v", p.Suggestions)
	}
	if fees := suggestion(p, KindFees, "Fees"); fees == nil || *fees.MonthlySavings != 25 {
		t.Errorf("want a fees suggestion saving 25, got %+v", fees)
	}
	if got := line(t, p, "Fees").Target; got != 0 {
		t.Errorf("fees target = %v, want 0", got)
	}
	// Groceries matched normal, but a quarter of months came in at 600 or
	// less, which is no saving against 600.
	if trim := suggestion(p, KindTrim, "Groceries"); trim != nil {
		t.Errorf("unexpected groceries trim: %+v", trim)
	}

	habit := suggestion(p, KindHabit, "")
	if habit == nil || !strings.Contains(habit.Title, "Noodle Cart") || *habit.MonthlySavings != 40 {
		t.Errorf("want a Noodle Cart habit saving 40, got %+v", habit)
	}
	subs := suggestion(p, KindSubscriptions, "")
	if subs == nil || !strings.Contains(subs.Detail, "Streamflix") || subs.MonthlySavings != nil {
		t.Errorf("want an unpriced subscriptions suggestion naming Streamflix, got %+v", subs)
	}
	if unc := suggestion(p, KindUncategorized, ""); unc == nil {
		t.Errorf("400 of 3840 uncategorized should be flagged")
	}
	if s := suggestion(p, KindShortfall, ""); s != nil {
		t.Errorf("no shortfall expected with a positive margin: %+v", s)
	}

	// Habits sit inside a category, so only the category savings count.
	if p.Totals.PotentialSavings != 375 {
		t.Errorf("potential savings = %v, want 375", p.Totals.PotentialSavings)
	}
	// Normal 3065, less the 0 fees target.
	if p.Totals.TargetSpend != 3065 {
		t.Errorf("target spend = %v, want 3065", p.Totals.TargetSpend)
	}

	found := false
	for _, n := range p.Notes {
		found = found || strings.Contains(n, "1 uncategorized credits")
	}
	if !found {
		t.Errorf("want a note about the skipped uncategorized credit, got %v", p.Notes)
	}
}

func TestBuildPlanTrimsTheLargestFlexibleCategories(t *testing.T) {
	c := planFixture()
	// A normal October for restaurants leaves room below it.
	var kept []*lunchmoney.Transaction
	for _, tx := range c.transactions {
		if tx.Date >= "2026-10-01" && *orZero(tx.CategoryID) == catRestaurants {
			continue
		}
		kept = append(kept, tx)
	}
	kept = append(kept, tx("2026-10-11", "Corner Bistro", 460, catRestaurants))
	c.transactions = kept

	p := buildFixturePlan(t, c)
	trim := suggestion(p, KindTrim, "Restaurants")
	if trim == nil || *trim.MonthlySavings != 60 {
		t.Fatalf("want a restaurants trim saving 60 (460 against a low of 400), got %+v", p.Suggestions)
	}
	if got := line(t, p, "Restaurants").Target; got != 400 {
		t.Errorf("restaurants target = %v, want 400", got)
	}
}

func orZero(p *int64) *int64 {
	if p == nil {
		return new(int64(0))
	}
	return p
}

func TestBuildPlanReportsAShortfallFirst(t *testing.T) {
	c := planFixture()
	for _, tx := range c.transactions {
		if tx.Payee == "Example Bank Mortgage" {
			tx.ToBase = 7000
		}
	}

	p := buildFixturePlan(t, c)
	if p.Totals.MarginNormal != -2065 {
		t.Fatalf("normal margin = %v, want -2065", p.Totals.MarginNormal)
	}
	if len(p.Suggestions) == 0 || p.Suggestions[0].Kind != KindShortfall {
		t.Errorf("want the shortfall first, got %+v", p.Suggestions)
	}
}

func TestBuildPlanPagesThroughTransactions(t *testing.T) {
	c := planFixture()
	c.pageSize = 7

	p := buildFixturePlan(t, c)
	if c.transactionGets < 2 {
		t.Fatalf("fetched in %d calls, want several pages", c.transactionGets)
	}
	if p.Totals.SpendReview != 3840 {
		t.Errorf("review spend = %v across pages, want 3840", p.Totals.SpendReview)
	}
}

func TestBuildPlanStartsTheBaselineWhereHistoryDoes(t *testing.T) {
	c := planFixture()
	var kept []*lunchmoney.Transaction
	for _, tx := range c.transactions {
		if tx.Date >= "2026-08-01" {
			kept = append(kept, tx)
		}
	}
	c.transactions = kept

	p := buildFixturePlan(t, c)
	if len(p.BaselineMonths) != 2 || p.BaselineMonths[0] != "2026-08" {
		t.Fatalf("baseline = %v, want August and September", p.BaselineMonths)
	}
	if got := line(t, p, "Restaurants").Normal; got != 450 {
		t.Errorf("restaurants normal = %v, want 450 from the two months with data", got)
	}
}

func TestBuildPlanHonoursPaycheckPatternAndFixedOverride(t *testing.T) {
	p, err := BuildPlan(context.Background(), planFixture(), PlanOptions{
		Review:        lastCompleteMonth(planNow),
		Now:           planNow,
		PaycheckMatch: regexp.MustCompile(`(?i)interest`),
		Fixed:         fixedSet("Groceries, restaurants"),
	})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Totals.PaycheckNormal != 2 || p.Totals.OtherIncomeNormal != 6000 {
		t.Errorf("paycheck/other = %v/%v, want 2/6000", p.Totals.PaycheckNormal, p.Totals.OtherIncomeNormal)
	}
	if !line(t, p, "Restaurants").Fixed || suggestion(p, KindOverNormal, "Restaurants") != nil {
		t.Errorf("a fixed category must not be suggested")
	}
	if !line(t, p, "Mortgage").Fixed {
		t.Errorf("debt stays fixed whatever the override says")
	}
}

func TestBuildPlanSurfacesFetchErrors(t *testing.T) {
	c := planFixture()
	c.transactionErr = errors.New("boom")
	if _, err := BuildPlan(context.Background(), c, PlanOptions{Review: planNow, Now: planNow}); err == nil {
		t.Fatal("want an error when transactions cannot be read")
	}
}

func TestPaycheckSourcesDropTraceNumbersAndNames(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	if len(p.Paychecks) != 1 {
		t.Fatalf("paychecks = %+v, want one source", p.Paychecks)
	}
	src := p.Paychecks[0]
	if src.Name != "ACME PAYROLL ADA" || src.Typical != 3000 || src.MonthlyNormal != 6000 || src.ReviewCount != 2 {
		t.Errorf("source = %+v", src)
	}
}

func TestPlanFor(t *testing.T) {
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name        string
		review, now time.Time
		want        string
	}{
		{"early in the month plans that month", day(10, 1), day(11, 3), "2026-11"},
		{"late in the month plans the next", day(10, 1), day(11, 25), "2026-12"},
		{"an old review month plans the current one", day(6, 1), day(11, 3), "2026-11"},
		{"reviewing the month in progress plans the next", day(11, 1), day(11, 3), "2026-12"},
	} {
		if got := planFor(tc.review, tc.now).Format(monthKey); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestQuantile(t *testing.T) {
	v := []float64{500, 300, 400, 600, 400, 500}
	if got := median(v); got != 450 {
		t.Errorf("median = %v, want 450", got)
	}
	if got := quantile(v, 0.25); got != 400 {
		t.Errorf("p25 = %v, want 400", got)
	}
	if got := median(nil); got != 0 {
		t.Errorf("median of nothing = %v, want 0", got)
	}
}

func planServer(c Fetcher, coach *Coach) http.Handler {
	return router(&Server{
		Log:    zap.NewNop().Sugar(),
		Client: c,
		Coach:  coach,
		Now:    fixedNow(planNow),
	}, http.NotFoundHandler())
}

func TestPlanPageRenders(t *testing.T) {
	coach := &Coach{Adviser: &fakeAdviser{}, Store: &MemoryStore{}, MaxCalls: 1}
	h := planServer(planFixture(), coach)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Plan for December 2026", "Reviewing October 2026", "Bring Restaurants back to $450",
		"Cook at home twice a week", "fake-model", "Noodle Cart", "Streamflix", "ACME PAYROLL ADA",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestPlanJSONDefaultsToLastCompleteMonth(t *testing.T) {
	h := planServer(planFixture(), nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/plan", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var p Plan
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Month != "2026-10" || p.Advice != nil || len(p.Suggestions) == 0 {
		t.Errorf("plan = month %s, advice %v, %d suggestions", p.Month, p.Advice, len(p.Suggestions))
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/plan?month=nope", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad month status = %d, want 400", w.Code)
	}
}

func TestPlanReportsUpstreamFailure(t *testing.T) {
	c := planFixture()
	c.transactionErr = errors.New("boom")
	h := planServer(c, nil)

	for _, path := range []string{"/", "/plan"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s status = %d, want 502", path, w.Code)
		}
	}
}

func TestAdviceRefreshRedirectsRelativelyAndRejectsCrossSite(t *testing.T) {
	adviser := &fakeAdviser{}
	h := planServer(planFixture(), &Coach{Adviser: adviser, Store: &MemoryStore{}, MaxCalls: 5})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/advice?month=2026-10", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "./?month=2026-10" {
		t.Fatalf("status = %d, location = %q", w.Code, w.Header().Get("Location"))
	}
	if adviser.calls != 1 {
		t.Errorf("adviser calls = %d, want 1", adviser.calls)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/advice?month=2026-10", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || adviser.calls != 1 {
		t.Errorf("cross-site post: status = %d, adviser calls = %d; want 403 and no new call", w.Code, adviser.calls)
	}
}

func TestOldDashboardMovedToDashboardPath(t *testing.T) {
	h := planServer(planFixture(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/dashboard", nil))
	if !strings.Contains(w.Body.String(), "<h1>acntng</h1>") {
		t.Errorf("/dashboard did not render the budget and debt page: %d", w.Code)
	}
}
