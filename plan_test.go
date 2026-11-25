package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	catLoans
	catFees
	catTransfer
	catSubscriptions
	catRent
	catHome
	catShopping
	catMedical
)

func planCategories() []*lunchmoney.Category {
	return []*lunchmoney.Category{
		{ID: catIncome, Name: "Income", IsIncome: true},
		{ID: catGroceries, Name: "Groceries"},
		{ID: catRestaurants, Name: "Restaurants"},
		{ID: catLoans, Name: "Personal Loans"},
		{ID: catFees, Name: "Fees"},
		{ID: catTransfer, Name: "Payment, Transfer", ExcludeFromBudget: true, ExcludeFromTotals: true},
		{ID: catSubscriptions, Name: "Subscriptions"},
		{ID: catRent, Name: "Rent and utilities"},
		{ID: catHome, Name: "Home improvement"},
		{ID: catShopping, Name: "Shopping"},
		{ID: catMedical, Name: "Medical"},
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

// planNow is late November, so December is planned and November counts
// toward lately, scaled by 30/25.
var planNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

var planSince = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// planFixture is a year of history with a move in September: new rent, an
// old water bill and alarm still running, restaurants up, foreign fees, a
// loan paid off in a lump and a sofa bought once.
func planFixture() *fakeClient {
	var txs []*lunchmoney.Transaction
	var months []string
	for m := time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC); !m.After(planNow); m = m.AddDate(0, 1, 0) {
		months = append(months, m.Format(monthKey))
	}
	for i, m := range months {
		cur := m == "2026-11"
		moved := m >= "2026-09"
		txs = append(txs,
			tx(m+"-15", "ACME PAYROLL 1115 ADA LOVELACE", -3000, catIncome),
			tx(m+"-03", "Old Town Water", 80, catRent),
			tx(m+"-04", "Alarmly", 30, catHome),
			tx(m+"-05", "Bankly", 300, catLoans),
			tx(m+"-06", "Streamflix", 15, catSubscriptions),
			tx(m+"-07", "Corner Store", 100, catShopping),
			tx(m+"-02", "Card Payment", 1500, catTransfer),
			tx(m+"-01", "MONTHLY SERVICE FEE", 15, catFees),
		)
		if !cur {
			txs = append(txs, tx(m+"-28", "ACME PAYROLL 1128 ADA LOVELACE", -3000, catIncome))
		}
		if m <= "2026-10" {
			txs = append(txs, tx(m+"-01", "Lendco", 500, catLoans))
		}

		groceries, meals := 300.0, 2
		if i%2 == 1 {
			groceries, meals = 500, 4
		}
		if moved {
			groceries, meals = 400, 7
		}
		if cur {
			meals = 5
		}
		txs = append(txs, tx(m+"-12", "Fresh Market", groceries, catGroceries))
		for d := range meals {
			txs = append(txs, tx(fmt.Sprintf("%s-%02d", m, 10+d), "Noodle Cart", 100, catRestaurants))
		}

		if moved {
			txs = append(txs,
				tx(m+"-01", "Harbor Lettings", 2000, catRent),
				tx(m+"-08", "FOREIGN TRANSACTION FEE", 10, catFees),
				tx(m+"-09", "FOREIGN TRANSACTION FEE", 10, catFees),
				tx(m+"-10", "FOREIGN TRANSACTION FEE", 10, catFees),
			)
		}
	}
	txs = append(txs,
		tx("2026-03-14", "Clinic Visit", 600, catMedical),
		tx("2026-10-20", "Lendco", 9000, catLoans),
		tx("2026-10-05", "Big Sofa Co", 1500, catShopping),
		tx("2026-10-21", "Mystery Shop", 400, 0),
		tx("2026-10-22", "Unlabelled Deposit", -90, 0),
	)
	pending := tx("2026-10-23", "Pending Diner", 999, catRestaurants)
	pending.IsPending = true
	txs = append(txs, pending)

	return &fakeClient{
		categories:   planCategories(),
		transactions: txs,
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Lendco personal loan", "0", "active"),
			manualAccount(2, "loan", "Bankly loan", "5000", "active"),
		},
	}
}

func planOpts(target float64) PlanOptions {
	return PlanOptions{Now: planNow, Since: planSince, SavingsTarget: target}
}

func buildFixturePlan(t *testing.T, c *fakeClient, opts PlanOptions) *Plan {
	t.Helper()
	p, err := BuildPlan(context.Background(), c, opts)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func capLine(t *testing.T, p *Plan, name string) CapLine {
	t.Helper()
	for _, l := range p.Caps {
		if l.Category == name {
			return l
		}
	}
	t.Fatalf("no cap %q in %+v", name, p.Caps)
	return CapLine{}
}

func bill(p *Plan, name string) *BillLine {
	for i, l := range p.Bills {
		if l.Name == name {
			return &p.Bills[i]
		}
	}
	return nil
}

func change(p *Plan, kind, category string) *Change {
	for i, c := range p.Changes {
		if c.Kind == kind && (category == "" || c.Category == category) {
			return &p.Changes[i]
		}
	}
	return nil
}

func TestBuildPlanPlansNextMonthFromLately(t *testing.T) {
	p := buildFixturePlan(t, planFixture(), planOpts(1000))

	if p.PlanFor != "2026-12" {
		t.Errorf("plan for = %s, want 2026-12", p.PlanFor)
	}
	if len(p.Months) != 12 || p.Months[0] != "2025-11" || p.Months[11] != "2026-10" {
		t.Errorf("months = %v", p.Months)
	}
	if strings.Join(p.Lately, ",") != "2026-09,2026-10,2026-11" {
		t.Errorf("lately = %v", p.Lately)
	}

	tot := p.Totals
	// Rent 2000, water 80, Bankly 300, a 600 doctor's bill spread over 12.
	if tot.Income != 6000 || tot.Bills != 2430 {
		t.Errorf("income/bills = %v/%v, want 6000/2430", tot.Income, tot.Bills)
	}
	// Groceries 400, restaurants 700, fees 45, shopping 100, alarm 30,
	// streaming 15. The sofa is a one-off and is not counted again.
	if tot.Spending != 1290 || tot.Unchanged != 2280 {
		t.Errorf("spending/unchanged = %v/%v, want 1290/2280", tot.Spending, tot.Unchanged)
	}
	if tot.Savings != 455 || tot.Planned != 2735 || tot.Short != 0 {
		t.Errorf("savings/planned/short = %v/%v/%v, want 455/2735/0", tot.Savings, tot.Planned, tot.Short)
	}
	if occ := bill(p, "Occasional medical"); occ == nil || occ.Monthly != 50 {
		t.Errorf("occasional medical = %+v, want 50", occ)
	}
	if b := bill(p, "Bankly"); b == nil || !b.Debt || b.Monthly != 300 {
		t.Errorf("Bankly = %+v, want a 300 debt bill", b)
	}
}

func TestBuildPlanChanges(t *testing.T) {
	p := buildFixturePlan(t, planFixture(), planOpts(1000))

	fx := change(p, KindFee, "")
	if fx == nil || fx.Saving != 30 || !strings.Contains(fx.Title, "foreign transaction") || !strings.Contains(fx.Detail, "9 foreign") {
		t.Errorf("foreign fee change = %+v", fx)
	}
	water, alarm := false, false
	for _, c := range p.Changes {
		if c.Kind == KindOldBill {
			water = water || (strings.Contains(c.Title, "Old Town Water") && c.Saving == 80)
			alarm = alarm || (strings.Contains(c.Title, "Alarmly") && c.Saving == 30)
		}
		if strings.Contains(c.Title, "Harbor Lettings") || strings.Contains(c.Title, "Streamflix") {
			t.Errorf("only home bills from before the move are old: %+v", c)
		}
	}
	if !water || !alarm {
		t.Errorf("want old water and alarm bills flagged, got %+v", p.Changes)
	}

	creep := change(p, KindCreep, "Restaurants")
	if creep == nil || creep.Saving != 300 || !strings.Contains(creep.Detail, "Noodle Cart") {
		t.Fatalf("want restaurants back to 400 saving 300, got %+v", creep)
	}
	if got := capLine(t, p, "Restaurants"); got.Lately != 700 || got.Usual != 400 || got.Cap != 400 {
		t.Errorf("restaurants cap = %+v", got)
	}
	if change(p, KindCut, "") != nil {
		t.Errorf("no cuts are needed when the target is met: %+v", p.Changes)
	}
	for i := 1; i < len(p.Changes); i++ {
		if p.Changes[i].Saving > p.Changes[i-1].Saving {
			t.Errorf("changes are not biggest first: %+v", p.Changes)
		}
	}
}

func TestBuildPlanCutsOnlyToReachTheTarget(t *testing.T) {
	p := buildFixturePlan(t, planFixture(), planOpts(3000))

	// 2735 leaves 265 to find. Restaurants has 200 of room down to its
	// leanest quarter, folded into its existing change; groceries gives 65.
	r := change(p, KindCreep, "Restaurants")
	if r == nil || r.Saving != 500 || r.Title != "Hold Restaurants to $200" {
		t.Errorf("restaurants = %+v, want one change to 200 saving 500", r)
	}
	g := change(p, KindCut, "Groceries")
	if g == nil || g.Saving != 65 || g.Title != "Hold Groceries to $335" {
		t.Errorf("groceries = %+v, want a 65 cut to 335", g)
	}
	if p.Totals.Planned != 3000 || p.Totals.Short != 0 {
		t.Errorf("planned/short = %v/%v", p.Totals.Planned, p.Totals.Short)
	}

	p = buildFixturePlan(t, planFixture(), planOpts(5000))
	if p.Totals.Short <= 0 || !strings.Contains(verdict(p), "short of your $5,000 target") || !strings.Contains(basis(p), "debt payments") {
		t.Errorf("an unreachable target should say so: %v, %q", p.Totals, verdict(p))
	}
}

func TestBuildPlanDropsPaidOffDebtAndOneOffs(t *testing.T) {
	p := buildFixturePlan(t, planFixture(), planOpts(1000))

	if bill(p, "Lendco") != nil {
		t.Errorf("a paid-off loan is still a bill: %+v", p.Bills)
	}
	if len(p.Stopped) != 1 || p.Stopped[0].Name != "Lendco" || p.Stopped[0].Monthly != 500 || p.Stopped[0].Reason != "paid off" {
		t.Errorf("stopped = %+v", p.Stopped)
	}
	got := map[string]string{}
	for _, o := range p.OneOffs {
		got[o.Payee] = o.Reason
	}
	if got["Lendco"] != "extra payment toward the balance" || got["Big Sofa Co"] != "one-time purchase" || len(got) != 2 {
		t.Errorf("one-offs = %+v", p.OneOffs)
	}
	if s := capLine(t, p, "Shopping"); s.Lately != 100 {
		t.Errorf("shopping lately = %v, want 100 without the sofa", s.Lately)
	}
}

func TestBuildPlanKeepsADebtStillOwed(t *testing.T) {
	c := planFixture()
	c.manualAccounts[0].Balance = "4000"

	p := buildFixturePlan(t, c, planOpts(1000))
	if b := bill(p, "Lendco"); b == nil || b.Monthly != 500 {
		t.Errorf("a loan with a balance left stays a bill after an extra payment: %+v", p.Bills)
	}
}

func TestBuildPlanWithoutSinceUsesTheLastThreeMonths(t *testing.T) {
	opts := planOpts(1000)
	opts.Since = time.Time{}
	p := buildFixturePlan(t, planFixture(), opts)

	if strings.Join(p.Lately, ",") != "2026-08,2026-09,2026-10,2026-11" {
		t.Errorf("lately = %v", p.Lately)
	}
	if change(p, KindOldBill, "") != nil {
		t.Errorf("old bills need a move date: %+v", p.Changes)
	}
}

func TestBuildPlanNotes(t *testing.T) {
	p := buildFixturePlan(t, planFixture(), planOpts(1000))
	joined := strings.Join(p.Notes, "\n")
	if !strings.Contains(joined, "1 uncategorized credits") || !strings.Contains(joined, "have no category") {
		t.Errorf("notes = %v", p.Notes)
	}
}

func TestBuildPlanPagesThroughTransactions(t *testing.T) {
	c := planFixture()
	c.pageSize = 17

	p := buildFixturePlan(t, c, planOpts(1000))
	if c.transactionGets < 2 {
		t.Fatalf("fetched in %d calls, want several pages", c.transactionGets)
	}
	if p.Totals.Planned != 2735 {
		t.Errorf("planned = %v across pages, want 2735", p.Totals.Planned)
	}
}

func TestBuildPlanSurvivesMissingLoans(t *testing.T) {
	c := planFixture()
	c.manualErr = errors.New("boom")
	p := buildFixturePlan(t, c, planOpts(1000))
	if !strings.Contains(strings.Join(p.Notes, "\n"), "loan balances could not be read") {
		t.Errorf("notes = %v", p.Notes)
	}
}

func TestBuildPlanSurfacesFetchErrors(t *testing.T) {
	c := planFixture()
	c.transactionErr = errors.New("boom")
	if _, err := BuildPlan(context.Background(), c, planOpts(1000)); err == nil {
		t.Fatal("want an error when transactions cannot be read")
	}
}

func TestBuildPlanWithoutHistory(t *testing.T) {
	p := buildFixturePlan(t, &fakeClient{categories: planCategories()}, planOpts(1000))
	if len(p.Months) != 0 || len(p.Changes) != 0 || len(p.Notes) != 1 {
		t.Errorf("plan = %+v", p)
	}
	if _, err := renderTemplate("plan.html", p); err != nil {
		t.Errorf("an empty plan must still render: %v", err)
	}
}

func TestFeeKind(t *testing.T) {
	for text, want := range map[string]string{
		"Interest Charge on Pay Over Time Purchases": feeInterest,
		"INTERNATIONAL PURCHASE TRANSACTION FEE":     feeForeign,
		"OVERDRAFT FEE FOR A TRANSACTION":            feeOverdraft,
		"RETURN PAYMENT FEE":                         feeOverdraft,
		"MONTHLY SERVICE FEE":                        feeService,
		"Bean Counter Coffee":                        "",
	} {
		if got := feeKind(text, "Coffee"); got != want {
			t.Errorf("feeKind(%q) = %q, want %q", text, got, want)
		}
	}
	if got := feeKind("Some Bank", "Fees"); got != feeOther {
		t.Errorf("anything in a fees category is a fee, got %q", got)
	}
}

func TestPayeeKeyDropsTraceNumbers(t *testing.T) {
	if a, b := payeeKey("WIDGET LENDING ACCT 261016 XXXXX4000 ADA"), payeeKey("Widget Lending"); a != b {
		t.Errorf("keys differ: %q and %q", a, b)
	}
	if got := displayName("Skyways 93201779988"); got != "Skyways" {
		t.Errorf("displayName = %q", got)
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

func planServer(c Fetcher) http.Handler {
	return router(&Server{
		Log:         zap.NewNop().Sugar(),
		Client:      c,
		PlanOptions: planOpts(1000),
		Now:         fixedNow(planNow),
	}, http.NotFoundHandler())
}

func TestPlanPageRenders(t *testing.T) {
	h := planServer(planFixture())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"December 2026", "Make the 5 changes below and December 2026 leaves $2,735.", "With no changes it leaves $2,280.", "What to change",
		"Bring Restaurants back to $400", "Cancel Old Town Water if it is for the old place",
		"Use a card with no foreign transaction fees", "Harbor Lettings", "Big Sofa Co", "Lendco", "paid off",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	for _, gone := range []string{"?month=", "Generate", "Reviewing"} {
		if strings.Contains(body, gone) {
			t.Errorf("page still has %q", gone)
		}
	}
}

func TestPlanJSON(t *testing.T) {
	h := planServer(planFixture())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/plan", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var p Plan
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.PlanFor != "2026-12" || p.Totals.Planned != 2735 || len(p.Changes) == 0 {
		t.Errorf("plan = %s, planned %v, %d changes", p.PlanFor, p.Totals.Planned, len(p.Changes))
	}
}

func TestPlanReportsUpstreamFailure(t *testing.T) {
	c := planFixture()
	c.transactionErr = errors.New("boom")
	h := planServer(c)

	for _, path := range []string{"/", "/plan"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s status = %d, want 502", path, w.Code)
		}
	}
}

func TestDashboardStillRenders(t *testing.T) {
	h := planServer(planFixture())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/dashboard", nil))
	if !strings.Contains(w.Body.String(), "<h1>acntng</h1>") {
		t.Errorf("/dashboard did not render the budget and debt page: %d", w.Code)
	}
}

func TestPrintPlan(t *testing.T) {
	var b strings.Builder
	if err := printPlan(&b, buildFixturePlan(t, planFixture(), planOpts(1000))); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"December 2026", "CHANGES", "Bring Restaurants back to $400", "NO LONGER DUE", "Lendco"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, b.String())
		}
	}
}

func TestPlanOptionsFromEnv(t *testing.T) {
	t.Setenv("ACNTNG_SINCE", "2026-09")
	t.Setenv("ACNTNG_SAVINGS_TARGET", "750")
	opts, err := planOptionsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Since.Equal(planSince) || opts.SavingsTarget != 750 || opts.HistoryMonths != defaultHistoryMonths {
		t.Errorf("opts = %+v", opts)
	}

	t.Setenv("ACNTNG_SINCE", "September")
	if _, err := planOptionsFromEnv(); err == nil {
		t.Error("want an error for a malformed ACNTNG_SINCE")
	}
}

func TestBuildPlanReservesForQuarterlyBills(t *testing.T) {
	c := planFixture()
	const catInsurance int64 = 60
	c.categories = append(c.categories, &lunchmoney.Category{ID: catInsurance, Name: "Insurance"})
	for _, d := range []string{"2025-12-10", "2026-03-10", "2026-06-10", "2026-09-10"} {
		c.transactions = append(c.transactions, tx(d, "Shield Mutual", 240, catInsurance))
	}
	opts := planOpts(1000)
	opts.Since = time.Time{}

	p := buildFixturePlan(t, c, opts)
	b := bill(p, "Shield Mutual, about every 3 months")
	if b == nil || b.Monthly != 80 {
		t.Errorf("want 960 a year set aside as 80 a month, got %+v", p.Bills)
	}
	for _, s := range p.Stopped {
		if s.Name == "Shield Mutual" {
			t.Errorf("a quarterly bill due next month was stopped: %+v", s)
		}
	}
}

func TestBuildPlanKeepsADebtWithUnknownBalance(t *testing.T) {
	c := planFixture()
	c.manualErr = errors.New("boom")
	p := buildFixturePlan(t, c, planOpts(1000))
	if b := bill(p, "Lendco"); b == nil || b.Monthly != 500 {
		t.Errorf("without a balance an extra payment is not a payoff: %+v", p.Bills)
	}
}

func TestBuildPlanBudgetRowsAddUp(t *testing.T) {
	for _, target := range []float64{1000, 3000, 5000} {
		p := buildFixturePlan(t, planFixture(), planOpts(target))
		left := p.Totals.Income
		for _, b := range p.Bills {
			left -= b.Plan
		}
		for _, c := range p.Caps {
			left -= c.Cap
		}
		if round2(left) != p.Totals.Planned || round2(p.Totals.Planned-p.Totals.Unchanged) != p.Totals.Savings {
			t.Errorf("target %v: rows leave %v, planned %v, savings %v", target, left, p.Totals.Planned, p.Totals.Savings)
		}
		saved := 0.0
		for i, c := range p.Changes {
			saved += c.Saving
			if i > 0 && c.Saving > p.Changes[i-1].Saving {
				t.Errorf("target %v: changes are not biggest first: %+v", target, p.Changes)
			}
		}
		if round2(saved) != p.Totals.Savings {
			t.Errorf("target %v: changes save %v, totals say %v", target, saved, p.Totals.Savings)
		}
	}
	if w := bill(buildFixturePlan(t, planFixture(), planOpts(1000)), "Old Town Water"); w == nil || w.Plan != 0 || w.Monthly != 80 {
		t.Errorf("a cancelled bill keeps its cost and plans zero: %+v", w)
	}
}

func TestFeeChangesNeverExceedTheFeesLine(t *testing.T) {
	c := planFixture()
	c.transactions = append(c.transactions,
		tx("2026-09-20", "MONTHLY SERVICE FEE", 100, catFees),
		tx("2026-10-20", "MONTHLY SERVICE FEE", 100, catFees),
		tx("2026-10-21", "FOREIGN TRANSACTION FEE", 100, catFees),
		tx("2026-11-21", "FOREIGN TRANSACTION FEE", 100, catFees),
	)
	p := buildFixturePlan(t, c, planOpts(1000))
	fees := 0.0
	for _, ch := range p.Changes {
		if ch.Kind == KindFee {
			fees += ch.Saving
		}
	}
	if line := capLine(t, p, feesName); round2(fees) > line.Lately || line.Cap < 0 {
		t.Errorf("fee changes save %v against a fees line of %+v", fees, line)
	}
}

func TestBuildPlanSinceThisMonthIgnoresEarlierMonths(t *testing.T) {
	opts := planOpts(1000)
	opts.Since = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	p := buildFixturePlan(t, planFixture(), opts)
	if strings.Join(p.Lately, ",") != "2026-11" {
		t.Errorf("lately = %v, want only November", p.Lately)
	}

	opts.Since = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	p = buildFixturePlan(t, planFixture(), opts)
	if len(p.Lately) != 0 || len(p.Changes) != 0 || !strings.Contains(strings.Join(p.Notes, " "), "ACNTNG_SINCE") {
		t.Errorf("a future Since should plan nothing and say why: %+v", p)
	}
}

func TestIncomeFollowsTheCurrentEmployer(t *testing.T) {
	c := planFixture()
	var kept []*lunchmoney.Transaction
	for _, x := range c.transactions {
		if strings.HasPrefix(x.Payee, "ACME PAYROLL") && x.Date >= "2026-08-01" {
			continue
		}
		kept = append(kept, x)
	}
	for _, m := range []string{"2026-08", "2026-09", "2026-10", "2026-11"} {
		kept = append(kept, tx(m+"-20", "NEWCO PAYROLL 77 ADA LOVELACE", -7000, catIncome))
	}
	c.transactions = kept
	opts := planOpts(1000)
	opts.Since = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	p := buildFixturePlan(t, c, opts)
	if len(p.Income) != 1 || p.Income[0].Name != "NEWCO PAYROLL ADA" || p.Income[0].Monthly != 7000 {
		t.Errorf("income = %+v, want only the new employer at 7000", p.Income)
	}
}

func TestOccasionalCostsIgnoreThePartialMonth(t *testing.T) {
	c := planFixture()
	c.transactions = append(c.transactions, tx("2026-11-03", "Clinic Visit", 600, catMedical))
	p := buildFixturePlan(t, c, planOpts(1000))
	if occ := bill(p, "Occasional medical"); occ == nil || occ.Monthly != 50 {
		t.Errorf("occasional medical = %+v, want 50 from complete months only", occ)
	}
}

func TestCutDetailsMatchTheirTitle(t *testing.T) {
	p := buildFixturePlan(t, planFixture(), planOpts(3000))
	g := change(p, KindCut, "Groceries")
	if g == nil || !strings.Contains(g.Detail, "Holding it to $335") {
		t.Errorf("groceries cut = %+v", g)
	}
	if r := change(p, KindCreep, "Restaurants"); r == nil || !strings.Contains(r.Detail, "hold it to $200") {
		t.Errorf("restaurants = %+v", r)
	}
}

func TestVerdictWithNothingToChangeButShort(t *testing.T) {
	p := &Plan{PlanFor: "2026-12", Totals: PlanTotals{Income: 6000, Bills: 5500, Unchanged: 500, Planned: 500, Target: 1000, Short: 500}}
	if got := verdict(p); !strings.Contains(got, "$500 short of your $1,000 target") {
		t.Errorf("verdict = %q", got)
	}
}
