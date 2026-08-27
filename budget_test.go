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

// budgetRow builds one Lunch Money budget row for the test month.
func budgetRow(id int, name string, budgeted, spent float64, opts ...func(*lunchmoney.Budget)) *lunchmoney.Budget {
	b := &lunchmoney.Budget{
		CategoryID:   id,
		CategoryName: name,
		Data: map[string]*lunchmoney.BudgetData{
			testMonth: {
				BudgetMonth:    testMonth,
				BudgetToBase:   budgeted,
				BudgetAmount:   json.Number("0"),
				BudgetCurrency: "usd",
				SpendingToBase: spent,
			},
		},
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

func asIncome(b *lunchmoney.Budget) { b.IsIncome = true }
func asGroup(b *lunchmoney.Budget)  { b.IsGroup = true }
func asExcluded(b *lunchmoney.Budget) {
	b.ExcludeFromBudget = true
}

func TestParseDebtCategories(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		got := parseDebtCategories("")
		for _, want := range []string{"mortgage", "student loans", "buy now pay later"} {
			if !got[want] {
				t.Errorf("default set is missing %q", want)
			}
		}
	})

	t.Run("override replaces defaults", func(t *testing.T) {
		got := parseDebtCategories(" Boat Loan , Airship Note ")
		if !got["boat loan"] || !got["airship note"] {
			t.Errorf("override not parsed: %v", got)
		}
		if got["mortgage"] {
			t.Error("override should replace the defaults, not extend them")
		}
	})

	t.Run("ignores empty entries", func(t *testing.T) {
		if got := parseDebtCategories("a loan,,  ,"); len(got) != 1 {
			t.Errorf("got %v, want just the one name", got)
		}
	})
}

func TestBuildBudgetReportClassifiesAndTotals(t *testing.T) {
	c := &fakeClient{budgets: []*lunchmoney.Budget{
		budgetRow(1, "Income", 0, -8000, asIncome),
		budgetRow(2, "Mortgage", 2000, 2000),
		budgetRow(3, "Student Loans", 1000, 400),
		budgetRow(4, "Groceries", 600, 250),
		budgetRow(5, "Restaurants and Delivery", 300, 450),
	}}

	rep, err := BuildBudgetReport(context.Background(), c, testNow, parseDebtCategories(""))
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
	c := &fakeClient{budgets: []*lunchmoney.Budget{
		budgetRow(1, "Mortgage", 2000, 2000),
		// Transfers and card payments are settlement, not spending; counting
		// them double-counts every purchase already in another category.
		budgetRow(2, "Payment, Transfer", 5000, 5000, asExcluded),
		// A group restates its children.
		budgetRow(3, "Everything", 9999, 9999, asGroup),
		// No budget and no activity is noise.
		budgetRow(4, "Dormant", 0, 0),
	}}

	rep, err := BuildBudgetReport(context.Background(), c, testNow, parseDebtCategories(""))
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
		c := &fakeClient{budgets: []*lunchmoney.Budget{
			budgetRow(1, "Income", 0, -5000, asIncome),
			budgetRow(2, "Groceries", 400, 100),
		}}

		rep, err := BuildBudgetReport(context.Background(), c, testNow, parseDebtCategories(""))
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
		c := &fakeClient{budgets: []*lunchmoney.Budget{
			// Mid-month: only part of the month's income has landed.
			budgetRow(1, "Income", 6000, -1000, asIncome),
			budgetRow(2, "Groceries", 400, 100),
		}}

		rep, err := BuildBudgetReport(context.Background(), c, testNow, parseDebtCategories(""))
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
	c := &fakeClient{}
	if _, err := BuildBudgetReport(context.Background(), c, testNow, nil); err != nil {
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
	c := &fakeClient{budgetsErr: errors.New("upstream is down")}
	if _, err := BuildBudgetReport(context.Background(), c, testNow, nil); err == nil {
		t.Fatal("want an error when budgets cannot be read")
	}
}

func TestBudgetLinePctAndOver(t *testing.T) {
	c := &fakeClient{budgets: []*lunchmoney.Budget{
		budgetRow(1, "Half spent", 1000, 500),
		budgetRow(2, "Overspent", 100, 250),
		// Spending with nothing budgeted: percent is unknowable, not zero.
		budgetRow(3, "Unbudgeted", 0, 75),
	}}

	rep, err := BuildBudgetReport(context.Background(), c, testNow, nil)
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
	c := &fakeClient{budgets: []*lunchmoney.Budget{
		budgetRow(1, "Mortgage", 2000, 2000),
	}}
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
	c := &fakeClient{
		budgets: []*lunchmoney.Budget{
			budgetRow(1, "Income", 0, -8000, asIncome),
			budgetRow(2, "Mortgage", 2000, 2000),
			budgetRow(3, "Groceries", 600, 700),
		},
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Example Note", Balance: "12345.0000", Currency: "usd", Status: "active"},
		},
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
	c := &fakeClient{
		budgets:   []*lunchmoney.Budget{budgetRow(1, "Mortgage", 2000, 2000)},
		assetsErr: errors.New("assets are down"),
	}
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
