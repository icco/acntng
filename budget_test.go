package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/icco/lunchmoney"
)

// testMonth is the budget-period key for testNow.
const testMonth = "2026-08-01"

func testBudgetFixture(categories []*lunchmoney.Category, summaryCats []*lunchmoney.SummaryCategory) *fakeClient {
	return &fakeClient{
		categories: categories,
		summary: &lunchmoney.BudgetSummary{
			Categories: summaryCats,
		},
	}
}

func categoryRow(id int64, name string, opts ...func(*lunchmoney.Category)) *lunchmoney.Category {
	c := &lunchmoney.Category{
		ID:   id,
		Name: name,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func summaryRow(id int64, budgeted, spent float64) *lunchmoney.SummaryCategory {
	b := budgeted
	return &lunchmoney.SummaryCategory{
		CategoryID: id,
		Totals: lunchmoney.SummaryCategoryTotal{
			Budgeted:      &b,
			OtherActivity: spent,
		},
		Occurrences: []lunchmoney.SummaryOccurrence{
			{
				BudgetedCurrency: "usd",
			},
		},
	}
}

func asIncome(c *lunchmoney.Category) { c.IsIncome = true }
func asGroup(c *lunchmoney.Category)  { c.IsGroup = true }
func asExcluded(c *lunchmoney.Category) {
	c.ExcludeFromBudget = true
}

func TestIsDebt(t *testing.T) {
	for _, tt := range []struct {
		name string
		want bool
	}{
		{"Mortgage", true},
		{"student loans", true},
		{"  Buy Now Pay Later  ", true},
		{"Groceries", false},
		{"", false},
	} {
		if got := isDebt(tt.name); got != tt.want {
			t.Errorf("isDebt(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBuildBudgetReportClassifiesAndTotals(t *testing.T) {
	c := testBudgetFixture(
		[]*lunchmoney.Category{
			categoryRow(1, "Income", asIncome),
			categoryRow(2, "Mortgage"),
			categoryRow(3, "Student Loans"),
			categoryRow(4, "Groceries"),
			categoryRow(5, "Restaurants and Delivery"),
		},
		[]*lunchmoney.SummaryCategory{
			summaryRow(1, 0, -8000),
			summaryRow(2, 2000, 2000),
			summaryRow(3, 1000, 400),
			summaryRow(4, 600, 250),
			summaryRow(5, 300, 450),
		},
	)

	rep, err := BuildBudgetReport(context.Background(), c, testNow)
	if err != nil {
		t.Fatalf("BuildBudgetReport: %v", err)
	}

	if got := rep.Month; got != "2026-08" {
		t.Errorf("month = %q, want 2026-08", got)
	}
	if len(rep.Income) != 1 || len(rep.Debt) != 2 || len(rep.Living) != 2 {
		t.Fatalf("classification = %d income, %d debt, %d living",
			len(rep.Income), len(rep.Debt), len(rep.Living))
	}

	tot := rep.Totals
	if tot.DebtBudgeted != 3000 {
		t.Errorf("debt budgeted = %v, want 3000", tot.DebtBudgeted)
	}
	if tot.LivingBudgeted != 900 {
		t.Errorf("living budgeted = %v, want 900", tot.LivingBudgeted)
	}
	if tot.OutflowBudgeted != 3900 {
		t.Errorf("outflow budgeted = %v, want 3900", tot.OutflowBudgeted)
	}
	// Income is reported as a credit; magnitude keeps the totals sane.
	if tot.IncomeActual != 8000 {
		t.Errorf("income actual = %v, want 8000 (absolute)", tot.IncomeActual)
	}
	if tot.PlannedSurplus != 4100 {
		t.Errorf("planned surplus = %v, want 4100", tot.PlannedSurplus)
	}
	// Only Restaurants is over: 450 spent against 300.
	if tot.CategoriesOver != 1 {
		t.Errorf("categories over = %d, want 1", tot.CategoriesOver)
	}
	if tot.DebtShare == nil || *tot.DebtShare != 0.38 {
		t.Errorf("debt share = %v, want 0.38", tot.DebtShare)
	}
	if rep.Currency != "USD" {
		t.Errorf("currency = %q, want USD", rep.Currency)
	}
}

func TestBuildBudgetReportSkipsNonSpending(t *testing.T) {
	c := testBudgetFixture(
		[]*lunchmoney.Category{
			categoryRow(1, "Mortgage"),
			// Transfers and card payments are settlement, not spending; counting
			// them double-counts every purchase already in another category.
			categoryRow(2, "Payment, Transfer", asExcluded),
			// A group restates its children.
			categoryRow(3, "Everything", asGroup),
			// No budget and no activity is noise.
			categoryRow(4, "Dormant"),
		},
		[]*lunchmoney.SummaryCategory{
			summaryRow(1, 2000, 2000),
			summaryRow(2, 5000, 5000),
			summaryRow(3, 9999, 9999),
			summaryRow(4, 0, 0),
		},
	)

	rep, err := BuildBudgetReport(context.Background(), c, testNow)
	if err != nil {
		t.Fatalf("BuildBudgetReport: %v", err)
	}

	if got := len(rep.Debt) + len(rep.Living) + len(rep.Income); got != 1 {
		t.Fatalf("kept %d lines, want only Mortgage", got)
	}
	if rep.Totals.OutflowBudgeted != 2000 {
		t.Errorf("outflow = %v, want 2000", rep.Totals.OutflowBudgeted)
	}

	var found bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "exclude from budgets") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a note about excluded categories, got %v", rep.Notes)
	}
}

func TestBuildBudgetReportIncomeBasis(t *testing.T) {
	t.Run("falls back to actual when no income is budgeted", func(t *testing.T) {
		c := testBudgetFixture(
			[]*lunchmoney.Category{
				categoryRow(1, "Income", asIncome),
				categoryRow(2, "Groceries"),
			},
			[]*lunchmoney.SummaryCategory{
				summaryRow(1, 0, -5000),
				summaryRow(2, 400, 100),
			},
		)

		rep, err := BuildBudgetReport(context.Background(), c, testNow)
		if err != nil {
			t.Fatalf("BuildBudgetReport: %v", err)
		}
		if rep.Totals.IncomeBasis != IncomeBasisActual {
			t.Errorf("basis = %q, want actual", rep.Totals.IncomeBasis)
		}
		if rep.Totals.PlannedSurplus != 4600 {
			t.Errorf("planned surplus = %v, want 4600", rep.Totals.PlannedSurplus)
		}
		var found bool
		for _, n := range rep.Notes {
			if strings.Contains(n, "no income budget is set") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a note explaining the fallback, got %v", rep.Notes)
		}
	})

	t.Run("prefers a budgeted figure when one exists", func(t *testing.T) {
		c := testBudgetFixture(
			[]*lunchmoney.Category{
				// Mid-month: only part of the month's income has landed.
				categoryRow(1, "Income", asIncome),
				categoryRow(2, "Groceries"),
			},
			[]*lunchmoney.SummaryCategory{
				summaryRow(1, 6000, -1000),
				summaryRow(2, 400, 100),
			},
		)

		rep, err := BuildBudgetReport(context.Background(), c, testNow)
		if err != nil {
			t.Fatalf("BuildBudgetReport: %v", err)
		}
		if rep.Totals.IncomeBasis != IncomeBasisBudgeted {
			t.Errorf("basis = %q, want budgeted", rep.Totals.IncomeBasis)
		}
		if rep.Totals.PlannedSurplus != 5600 {
			t.Errorf("planned surplus = %v, want 5600 (against budgeted income)", rep.Totals.PlannedSurplus)
		}
	})
}

func TestBuildBudgetReportRequestsTheCurrentMonth(t *testing.T) {
	c := testBudgetFixture(nil, nil)
	if _, err := BuildBudgetReport(context.Background(), c, testNow); err != nil {
		t.Fatalf("BuildBudgetReport: %v", err)
	}

	if c.gotBudgetFilters == nil {
		t.Fatal("no budget filters were sent")
	}
	if c.gotBudgetFilters.StartDate != testMonth {
		t.Errorf("start = %q, want %q", c.gotBudgetFilters.StartDate, testMonth)
	}
	if c.gotBudgetFilters.EndDate != "2026-08-31" {
		t.Errorf("end = %q, want 2026-08-31", c.gotBudgetFilters.EndDate)
	}
}

func TestBuildBudgetReportPropagatesError(t *testing.T) {
	c := &fakeClient{summaryErr: errors.New("upstream is down")}
	if _, err := BuildBudgetReport(context.Background(), c, testNow); err == nil {
		t.Fatal("want an error when budgets cannot be read")
	}
}

func TestBudgetLinePctAndOver(t *testing.T) {
	c := testBudgetFixture(
		[]*lunchmoney.Category{
			categoryRow(1, "Half spent"),
			categoryRow(2, "Overspent"),
			// Spending with nothing budgeted: percent is unknowable, not zero.
			categoryRow(3, "Unbudgeted"),
		},
		[]*lunchmoney.SummaryCategory{
			summaryRow(1, 1000, 500),
			summaryRow(2, 100, 250),
			summaryRow(3, 0, 75),
		},
	)

	rep, err := BuildBudgetReport(context.Background(), c, testNow)
	if err != nil {
		t.Fatalf("BuildBudgetReport: %v", err)
	}

	byName := map[string]BudgetLine{}
	for _, l := range rep.Living {
		byName[l.Name] = l
	}

	if l := byName["Half spent"]; l.PctUsed == nil || *l.PctUsed != 50 || l.Over() {
		t.Errorf("half spent = %+v", l)
	}
	if l := byName["Overspent"]; !l.Over() || l.Remaining != -150 {
		t.Errorf("overspent = %+v", l)
	}
	if l := byName["Unbudgeted"]; l.PctUsed != nil {
		t.Errorf("unbudgeted pct = %v, want nil so it differs from 0%%", *l.PctUsed)
	}
}

func TestHandleBudgetReturnsJSON(t *testing.T) {
	c := testBudgetFixture(
		[]*lunchmoney.Category{categoryRow(1, "Mortgage")},
		[]*lunchmoney.SummaryCategory{summaryRow(1, 2000, 2000)},
	)
	h := testServer(c, fixedNow(testNow))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/budget", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want json", ct)
	}

	var rep BudgetReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("could not decode %s: %v", w.Body.String(), err)
	}
	if rep.Totals.DebtBudgeted != 2000 {
		t.Errorf("debt budgeted = %v, want 2000", rep.Totals.DebtBudgeted)
	}
}

func TestHandleDashboardRendersHTML(t *testing.T) {
	c := testBudgetFixture(
		[]*lunchmoney.Category{
			categoryRow(1, "Income", asIncome),
			categoryRow(2, "Mortgage"),
			categoryRow(3, "Groceries"),
		},
		[]*lunchmoney.SummaryCategory{
			summaryRow(1, 0, -8000),
			summaryRow(2, 2000, 2000),
			summaryRow(3, 600, 700),
		},
	)
	c.manualAccounts = []*lunchmoney.ManualAccount{
		manualAccount(1, "loan", "Example Note", "12345.0000", "active"),
	}
	h := testServer(c, fixedNow(testNow))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want html", ct)
	}

	body := w.Body.String()
	for _, want := range []string{
		"August 2026", // month header
		"Mortgage",
		"Groceries",
		"Example Note",
		"12,345", // loan balance, thousands separated
		"2,000",  // mortgage budget
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q", want)
		}
	}
	// Groceries is overspent, so its figure must carry the over-budget class.
	if !strings.Contains(body, `class="num bad"`) {
		t.Error("expected an over-budget cell to be marked")
	}
}

func TestHandleDashboardSurvivesOneUpstreamFailure(t *testing.T) {
	// Loans fail, budget succeeds: the page should still render the budget.
	c := testBudgetFixture(
		[]*lunchmoney.Category{categoryRow(1, "Mortgage")},
		[]*lunchmoney.SummaryCategory{summaryRow(1, 2000, 2000)},
	)
	c.manualErr = errors.New("assets are down")
	h := testServer(c, fixedNow(testNow))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Mortgage") {
		t.Error("budget half should still render when loans fail")
	}
	if !strings.Contains(body, "could not read loans") {
		t.Error("expected the loan failure to be reported inline")
	}
}

func TestMoneyFormatting(t *testing.T) {
	for _, tt := range []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
		{-1500, "−1,500"},
		{99.6, "100"},
	} {
		if got := money(tt.in); got != tt.want {
			t.Errorf("money(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
