package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/icco/lunchmoney"
)

// fakeClient serves canned Lunch Money data.
type fakeClient struct {
	manualAccounts []*lunchmoney.ManualAccount
	plaid          []*lunchmoney.PlaidAccount
	recurring      []*lunchmoney.RecurringItem
	summary        *lunchmoney.BudgetSummary
	categories     []*lunchmoney.Category

	manualErr     error
	plaidErr      error
	recurringErr  error
	summaryErr    error
	categoriesErr error

	gotFilters         *lunchmoney.RecurringItemFilters
	gotBudgetFilters   *lunchmoney.BudgetFilters
	gotCategoryFilters *lunchmoney.CategoryFilters
}

func (f *fakeClient) GetManualAccounts(context.Context) ([]*lunchmoney.ManualAccount, error) {
	return f.manualAccounts, f.manualErr
}

func (f *fakeClient) GetPlaidAccounts(context.Context) ([]*lunchmoney.PlaidAccount, error) {
	return f.plaid, f.plaidErr
}

func (f *fakeClient) GetRecurringItems(_ context.Context, filters *lunchmoney.RecurringItemFilters) ([]*lunchmoney.RecurringItem, error) {
	f.gotFilters = filters
	return f.recurring, f.recurringErr
}

func (f *fakeClient) GetBudgetSummary(_ context.Context, filters *lunchmoney.BudgetFilters) (*lunchmoney.BudgetSummary, error) {
	f.gotBudgetFilters = filters
	return f.summary, f.summaryErr
}

func (f *fakeClient) GetCategories(_ context.Context, filters *lunchmoney.CategoryFilters) ([]*lunchmoney.Category, error) {
	f.gotCategoryFilters = filters
	return f.categories, f.categoriesErr
}

func ptr[T any](v T) *T { return &v }

func manualAccount(id int64, accountType, name, balance, status string) *lunchmoney.ManualAccount {
	return &lunchmoney.ManualAccount{
		ID:       id,
		Type:     accountType,
		Name:     name,
		Balance:  balance,
		Currency: "usd",
		Status:   status,
	}
}

func plaidAccount(id int64, accountType, subtype, name, balance, status string) *lunchmoney.PlaidAccount {
	return &lunchmoney.PlaidAccount{
		ID:       id,
		Type:     accountType,
		Subtype:  subtype,
		Name:     name,
		Balance:  balance,
		Currency: "usd",
		Status:   status,
	}
}

func recurringItem(id int64, payee, amount, granularity string, quantity int64, manualID, plaidID *int64) *lunchmoney.RecurringItem {
	return &lunchmoney.RecurringItem{
		ID: id,
		TransactionCriteria: lunchmoney.RecurringCriteria{
			Payee:           payee,
			Amount:          amount,
			Currency:        "usd",
			Granularity:     granularity,
			Quantity:        quantity,
			ManualAccountID: manualID,
			PlaidAccountID:  plaidID,
		},
	}
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Student Loan", "12000.0000", "active"),
			manualAccount(2, "cash", "Checking", "500.0000", "active"),
			manualAccount(3, "other liability", "Owed Mom", "300.0000", "active"),
			manualAccount(4, "loan", "Paid Off", "0.0000", "closed"),
		},
		plaid: []*lunchmoney.PlaidAccount{
			plaidAccount(10, "loan", "mortgage", "Mortgage", "250000.0000", "active"),
			plaidAccount(11, "credit", "credit card", "Amex", "1200.0000", "active"),
			plaidAccount(12, "depository", "", "Savings", "9000.0000", "active"),
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
	// Liquid cash: Checking (500) + Savings (9000) = 9500
	if rep.Totals.LiquidCash != 9500 {
		t.Errorf("totals.liquid_cash = %v, want 9500", rep.Totals.LiquidCash)
	}
	if rep.Totals.NetDebt != 252500 {
		t.Errorf("totals.net_debt = %v, want 252500", rep.Totals.NetDebt)
	}
}

func TestBuildReportOptInTypes(t *testing.T) {
	c := &fakeClient{
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(3, "other liability", "Owed Mom", "300.0000", "active"),
		},
		plaid: []*lunchmoney.PlaidAccount{
			plaidAccount(11, "credit", "", "Amex", "1200.0000", "active"),
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Car Loan", "9000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(100, "Acme Motor Credit", "450.00", "month", 1, ptr(int64(1)), nil),
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

	if c.gotFilters == nil || c.gotFilters.StartDate != "2026-08-01" {
		t.Errorf("recurring filters = %+v, want start_date 2026-08-01", c.gotFilters)
	}
}

func TestMonthlyPaymentFromPayeeMatch(t *testing.T) {
	// The realistic case: the recurring expense is booked against the
	// checking account it is paid from, not the loan.
	c := &fakeClient{
		manualAccounts: []*lunchmoney.ManualAccount{
			{ID: 1, Type: "loan", Name: "Meridian", InstitutionName: "Meridian", Balance: "22000.0000", Currency: "usd", Status: "active"},
		},
		plaid: []*lunchmoney.PlaidAccount{
			{ID: 50, Type: "depository", Name: "Checking", Balance: "1000.0000", Currency: "usd", Status: "active"},
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(101, "Meridian Student Loan", "310.00", "month", 1, nil, ptr(int64(50))),
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Mystery Debt", "500.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(102, "Netflix", "15.99", "month", 1, nil, ptr(int64(99))),
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Biweekly Loan", "1000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(103, "Lender", "100.00", "week", 2, ptr(int64(1)), nil),
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Loan", "1000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(104, "Lender", "1000.00", "once", 1, ptr(int64(1)), nil),
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Car Loan", "9000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(105, "Lender", "450.00", "month", 1, ptr(int64(1)), nil),
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Loan", "1000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(106, "Lender", "300.00", "month", 1, ptr(int64(1)), nil),
			recurringItem(107, "Lender extra", "600.00", "month", 6, ptr(int64(1)), nil),
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Loan", "1000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(108, "Lender", "-450.00", "month", 1, ptr(int64(1)), nil),
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
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Loan", "1000.0000", "active"),
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
	c := &fakeClient{manualErr: errors.New("401 Unauthorized")}

	if _, err := BuildReport(context.Background(), c, testNow, Options{}); err == nil {
		t.Fatal("want an error when assets cannot be read")
	}
}

func TestMixedCurrencyIsFlagged(t *testing.T) {
	c := &fakeClient{
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "US Loan", "1000.0000", "active"),
			manualAccount(2, "loan", "EU Loan", "2000.0000", "active"),
		},
	}
	c.manualAccounts[1].Currency = "eur"

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

func TestPayeeMatchIgnoresShortStringsBothWays(t *testing.T) {
	// A short loan name must not substring-match an unrelated payee...
	l := &Loan{Name: "Car", Source: SourceAsset}
	r := &lunchmoney.RecurringItem{
		TransactionCriteria: lunchmoney.RecurringCriteria{Payee: "Carwash Monthly"},
	}
	if got := payeeMatchScore(l, r); got != 0 {
		t.Errorf("short loan name scored %d, want 0", got)
	}

	// ...and neither must a short payee against a long loan name.
	l = &Loan{Name: "Meridian US Loan", Source: SourceAsset}
	r = &lunchmoney.RecurringItem{
		TransactionCriteria: lunchmoney.RecurringCriteria{Payee: "US"},
	}
	if got := payeeMatchScore(l, r); got != 0 {
		t.Errorf("short payee scored %d, want 0", got)
	}
}

func TestPayeeMatchScorePrefersSpecificName(t *testing.T) {
	r := &lunchmoney.RecurringItem{
		TransactionCriteria: lunchmoney.RecurringCriteria{Payee: "Northgate"},
	}

	short := payeeMatchScore(&Loan{Name: "Northgate", Source: SourceAsset}, r)
	long := payeeMatchScore(&Loan{Name: "Northgate Auto", Source: SourceAsset}, r)

	if short == 0 || long == 0 {
		t.Fatalf("both should match: short=%d long=%d", short, long)
	}
	if long <= short {
		t.Errorf("longer name scored %d, want more than %d", long, short)
	}
}

func TestPayeeMatchNotDoubleCounted(t *testing.T) {
	// Two loans at one institution with a single recurring payee. Crediting
	// both would double the reported monthly total.
	c := &fakeClient{
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Northgate Auto", "9000.0000", "active"),
			manualAccount(2, "loan", "Northgate Student", "22000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(200, "Northgate", "400.00", "month", 1, nil, ptr(int64(99))),
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	if rep.Totals.MonthlyPayment != 400 {
		t.Errorf("total monthly payment = %v, want 400 (counted once)", rep.Totals.MonthlyPayment)
	}

	withPayment := 0
	for _, l := range rep.Loans {
		if l.MonthlyPayment != nil {
			withPayment++
		}
	}
	if withPayment != 1 {
		t.Errorf("%d loans got the payment, want exactly 1", withPayment)
	}
}

func TestAmbiguousPayeeIsReportedNotGuessed(t *testing.T) {
	// Two loans whose names match the payee equally well. Picking one at
	// random would be a silent coin flip, so neither is credited.
	c := &fakeClient{
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Northgate", "9000.0000", "active"),
			manualAccount(2, "loan", "Northgate", "22000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(201, "Northgate", "400.00", "month", 1, nil, ptr(int64(99))),
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	if rep.Totals.MonthlyPayment != 0 || rep.Totals.LoansMissingPayment != 2 {
		t.Errorf("totals = %+v, want no payment assigned", rep.Totals)
	}

	var flagged bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "more than one loan") {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("want a note about the ambiguous payee, got %v", rep.Notes)
	}
}

func TestAccountLinkBeatsPayeeMatch(t *testing.T) {
	// An ID-linked expense is authoritative; a payee-matching expense must
	// not also pile onto that loan.
	c := &fakeClient{
		manualAccounts: []*lunchmoney.ManualAccount{
			manualAccount(1, "loan", "Harborview Mortgage", "250000.0000", "active"),
		},
		recurring: []*lunchmoney.RecurringItem{
			recurringItem(202, "Harborview", "1500.00", "month", 1, ptr(int64(1)), nil),
			recurringItem(203, "Harborview Mortgage", "1500.00", "month", 1, nil, ptr(int64(99))),
		},
	}

	rep, err := BuildReport(context.Background(), c, testNow, Options{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}

	l := rep.Loans[0]
	if l.PaymentSource != PaymentSourceAccountLink {
		t.Errorf("payment source = %q, want %q", l.PaymentSource, PaymentSourceAccountLink)
	}
	if *l.MonthlyPayment != 1500 {
		t.Errorf("monthly payment = %v, want 1500 (not doubled)", *l.MonthlyPayment)
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Meridian", "meridian"},
		{"Northgate - Auto #1234", "northgateauto1234"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalize(tt.in); got != tt.want {
			t.Errorf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
