package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/icco/lunchmoney"
)

// fakeClient serves canned Lunch Money data.
type fakeClient struct {
	assets       []*lunchmoney.Asset
	plaid        []*lunchmoney.PlaidAccount
	recurring    []*lunchmoney.RecurringExpense
	assetsErr    error
	plaidErr     error
	recurringErr error

	gotFilters *lunchmoney.RecurringExpenseFilters
}

func (f *fakeClient) GetAssets(context.Context) ([]*lunchmoney.Asset, error) {
	return f.assets, f.assetsErr
}

func (f *fakeClient) GetPlaidAccounts(context.Context) ([]*lunchmoney.PlaidAccount, error) {
	return f.plaid, f.plaidErr
}

func (f *fakeClient) GetRecurringExpenses(_ context.Context, filters *lunchmoney.RecurringExpenseFilters) ([]*lunchmoney.RecurringExpense, error) {
	f.gotFilters = filters
	return f.recurring, f.recurringErr
}

var testNow = time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)

func TestMonthlyFactor(t *testing.T) {
	tests := []struct {
		cadence string
		want    float64
		ok      bool
	}{
		{"monthly", 1, true},
		{"Monthly", 1, true},
		{" monthly ", 1, true},
		{"weekly", 52.0 / 12.0, true},
		{"every 2 weeks", 26.0 / 12.0, true},
		{"twice a month", 2, true},
		{"every 2 months", 0.5, true},
		{"every 3 months", 1.0 / 3.0, true},
		{"every 4 months", 0.25, true},
		{"twice a year", 1.0 / 6.0, true},
		{"yearly", 1.0 / 12.0, true},
		// "once" is a one-off, not a monthly obligation.
		{"once", 0, false},
		{"", 0, false},
		{"every 17 fortnights", 0, false},
	}

	for _, tt := range tests {
		got, ok := monthlyFactor(tt.cadence)
		if ok != tt.ok {
			t.Errorf("monthlyFactor(%q) ok = %v, want %v", tt.cadence, ok, tt.ok)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("monthlyFactor(%q) = %v, want %v", tt.cadence, got, tt.want)
		}
	}
}

func TestParseAmountKeepsCents(t *testing.T) {
	// lunchmoney.ParseCurrency does int64(100*f) and truncates; this must not.
	tests := []struct {
		in   string
		want float64
	}{
		{"12345.6700", 12345.67},
		{"12345.679", 12345.68},
		{"0.0000", 0},
		{" 900.10 ", 900.10},
		{"not a number", 0},
		{"", 0},
	}

	for _, tt := range tests {
		if got := parseAmount(tt.in); got != tt.want {
			t.Errorf("parseAmount(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestBuildReportFiltersToLoansOnly(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Student Loan", Balance: "12000.0000", Currency: "usd", Status: "active"},
			{ID: 2, TypeName: "cash", Name: "Checking", Balance: "500.0000", Currency: "usd", Status: "active"},
			{ID: 3, TypeName: "other liability", Name: "Owed Mom", Balance: "300.0000", Currency: "usd", Status: "active"},
			{ID: 4, TypeName: "loan", Name: "Paid Off", Balance: "0.0000", Currency: "usd", Status: "closed"},
		},
		plaid: []*lunchmoney.PlaidAccount{
			{ID: 10, Type: "loan", Subtype: "mortgage", Name: "Mortgage", Balance: "250000.0000", Currency: "usd", Status: "active"},
			{ID: 11, Type: "credit", Subtype: "credit card", Name: "Amex", Balance: "1200.0000", Currency: "usd", Status: "active"},
			{ID: 12, Type: "depository", Name: "Savings", Balance: "9000.0000", Currency: "usd", Status: "active"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	got := map[string]bool{}
	for _, l := range rep.Loans {
		got[l.ID] = true
	}

	if len(got) != 2 || !got["asset:1"] || !got["plaid:10"] {
		t.Fatalf("default loans = %v, want only asset:1 and plaid:10", got)
	}

	// Highest balance first.
	if rep.Loans[0].ID != "plaid:10" {
		t.Errorf("loans[0] = %q, want plaid:10 (sorted by balance desc)", rep.Loans[0].ID)
	}

	if rep.Totals.Balance != 262000 {
		t.Errorf("totals.balance = %v, want 262000", rep.Totals.Balance)
	}
}

func TestBuildReportOptInTypes(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 3, TypeName: "other liability", Name: "Owed Mom", Balance: "300.0000", Currency: "usd", Status: "active"},
		},
		plaid: []*lunchmoney.PlaidAccount{
			{ID: 11, Type: "credit", Name: "Amex", Balance: "1200.0000", Currency: "usd", Status: "active"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{IncludeCredit: true, IncludeLiabilities: true})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	if rep.Totals.Count != 2 {
		t.Fatalf("count = %d, want 2; loans = %+v", rep.Totals.Count, rep.Loans)
	}
}

func TestMonthlyPaymentFromAccountLink(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Car Loan", Balance: "9000.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringExpense{
			{ID: 100, AssetID: 1, Payee: "Toyota Financial", Amount: "450.00", Currency: "usd", Cadence: "monthly"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	l := rep.Loans[0]
	if l.MonthlyPayment == nil || *l.MonthlyPayment != 450 {
		t.Fatalf("monthly payment = %v, want 450", l.MonthlyPayment)
	}
	if l.PaymentSource != PaymentSourceAccountLink {
		t.Errorf("payment source = %q, want %q", l.PaymentSource, PaymentSourceAccountLink)
	}
	if rep.Totals.LoansMissingPayment != 0 {
		t.Errorf("missing = %d, want 0", rep.Totals.LoansMissingPayment)
	}

	// The API needs the reporting month to scope recurring expenses.
	if c.gotFilters == nil || c.gotFilters.StartDate != "2026-08-01" {
		t.Errorf("recurring filters = %+v, want start_date 2026-08-01", c.gotFilters)
	}
}

func TestMonthlyPaymentFromPayeeMatch(t *testing.T) {
	// The realistic case: the recurring expense is booked against the
	// checking account it is paid from, not the loan.
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Sallie Mae", InstitutionName: "Sallie Mae", Balance: "22000.0000", Currency: "usd", Status: "active"},
		},
		plaid: []*lunchmoney.PlaidAccount{
			{ID: 50, Type: "depository", Name: "Checking", Balance: "1000.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringExpense{
			{ID: 101, PlaidAccountID: 50, Payee: "Sallie Mae Student Loan", Amount: "310.00", Currency: "usd", Cadence: "monthly"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	l := rep.Loans[0]
	if l.MonthlyPayment == nil || *l.MonthlyPayment != 310 {
		t.Fatalf("monthly payment = %v, want 310", l.MonthlyPayment)
	}
	if l.PaymentSource != PaymentSourcePayeeMatch {
		t.Errorf("payment source = %q, want %q", l.PaymentSource, PaymentSourcePayeeMatch)
	}
}

func TestMissingPaymentIsNullNotZero(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Mystery Debt", Balance: "500.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringExpense{
			{ID: 102, PlaidAccountID: 99, Payee: "Netflix", Amount: "15.99", Currency: "usd", Cadence: "monthly"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	l := rep.Loans[0]
	if l.MonthlyPayment != nil {
		t.Fatalf("monthly payment = %v, want nil", *l.MonthlyPayment)
	}
	if l.PaymentSource != PaymentSourceNone {
		t.Errorf("payment source = %q, want %q", l.PaymentSource, PaymentSourceNone)
	}
	if rep.Totals.LoansMissingPayment != 1 {
		t.Errorf("missing = %d, want 1", rep.Totals.LoansMissingPayment)
	}
	if len(rep.Notes) == 0 {
		t.Error("want a note explaining the missing payment")
	}
}

func TestCadenceNormalizedToMonthly(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Biweekly Loan", Balance: "1000.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringExpense{
			{ID: 103, AssetID: 1, Payee: "Lender", Amount: "100.00", Currency: "usd", Cadence: "every 2 weeks"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	// 100 * 26/12 = 216.666... -> 216.67
	if got := *rep.Loans[0].MonthlyPayment; got != 216.67 {
		t.Errorf("monthly payment = %v, want 216.67", got)
	}
}

func TestOnceCadenceExcluded(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Loan", Balance: "1000.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringExpense{
			{ID: 104, AssetID: 1, Payee: "Lender", Amount: "1000.00", Currency: "usd", Cadence: "once"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	if rep.Loans[0].MonthlyPayment != nil {
		t.Errorf("monthly payment = %v, want nil for a one-off", *rep.Loans[0].MonthlyPayment)
	}
}

func TestOverrideWins(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Car Loan", Balance: "9000.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringExpense{
			{ID: 105, AssetID: 1, Payee: "Lender", Amount: "450.00", Currency: "usd", Cadence: "monthly"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{
		Overrides: map[string]float64{"asset:1": 512.34},
	})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	l := rep.Loans[0]
	if l.MonthlyPayment == nil || *l.MonthlyPayment != 512.34 {
		t.Fatalf("monthly payment = %v, want 512.34", l.MonthlyPayment)
	}
	if l.PaymentSource != PaymentSourceOverride {
		t.Errorf("payment source = %q, want %q", l.PaymentSource, PaymentSourceOverride)
	}
}

func TestMultiplePaymentsSum(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Loan", Balance: "1000.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringExpense{
			{ID: 106, AssetID: 1, Payee: "Lender", Amount: "300.00", Currency: "usd", Cadence: "monthly"},
			{ID: 107, AssetID: 1, Payee: "Lender extra", Amount: "600.00", Currency: "usd", Cadence: "twice a year"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	// 300 + 600/6 = 400
	if got := *rep.Loans[0].MonthlyPayment; got != 400 {
		t.Errorf("monthly payment = %v, want 400", got)
	}
	if len(rep.Loans[0].Payments) != 2 {
		t.Errorf("payments = %d, want 2", len(rep.Loans[0].Payments))
	}
}

func TestNegativeAmountTreatedAsMagnitude(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Loan", Balance: "1000.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringExpense{
			{ID: 108, AssetID: 1, Payee: "Lender", Amount: "-450.00", Currency: "usd", Cadence: "monthly"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	if got := *rep.Loans[0].MonthlyPayment; got != 450 {
		t.Errorf("monthly payment = %v, want 450", got)
	}
}

func TestRecurringFailureDegradesGracefully(t *testing.T) {
	// Balances are the primary answer; losing recurring expenses must not
	// take the whole report down.
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "Loan", Balance: "1000.0000", Currency: "usd", Status: "active"},
		},
		recurringErr: errors.New("429 Too Many Requests"),
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport should tolerate a recurring failure, got %v", err)
	}

	if rep.Totals.Count != 1 || rep.Totals.Balance != 1000 {
		t.Errorf("balances lost: %+v", rep.Totals)
	}
	if len(rep.Notes) == 0 {
		t.Error("want a note recording the recurring failure")
	}
}

func TestAssetsFailureIsFatal(t *testing.T) {
	c := &fakeClient{assetsErr: errors.New("401 Unauthorized")}

	if _, err := BuildReport(context.Background(), c, testNow, Options{}); err == nil {
		t.Fatal("want an error when assets cannot be read")
	}
}

func TestMixedCurrencyIsFlagged(t *testing.T) {
	c := &fakeClient{
		assets: []*lunchmoney.Asset{
			{ID: 1, TypeName: "loan", Name: "US Loan", Balance: "1000.0000", Currency: "usd", Status: "active"},
			{ID: 2, TypeName: "loan", Name: "EU Loan", Balance: "2000.0000", Currency: "eur", Status: "active"},
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	if rep.Currency != "" {
		t.Errorf("currency = %q, want empty for a mixed-currency report", rep.Currency)
	}

	var found bool
	for _, n := range rep.Notes {
		if len(n) > 0 && n[:5] == "loans" {
			found = true
		}
	}
	if !found {
		t.Errorf("want a mixed-currency note, got %v", rep.Notes)
	}
}

func TestPayeeMatchIgnoresShortNames(t *testing.T) {
	// A 3-character loan name must not substring-match unrelated payees.
	l := &Loan{Name: "Car", Source: SourceAsset}
	r := &lunchmoney.RecurringExpense{Payee: "Carwash Monthly"}

	if payeeMatches(l, r) {
		t.Error("a 3-character name should not payee-match")
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Sallie Mae", "salliemae"},
		{"Wells Fargo - Auto #1234", "wellsfargoauto1234"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalize(tt.in); got != tt.want {
			t.Errorf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
