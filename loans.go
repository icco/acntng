// Package main implements acntng, a small JSON service that reports the loans
// tracked in a Lunch Money account along with their balances and the monthly
// payment servicing each one.
package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/icco/lunchmoney"
)

// Source says which Lunch Money collection a loan record came from.
type Source string

const (
	// SourceAsset is a manually managed Lunch Money asset.
	SourceAsset Source = "asset"
	// SourcePlaid is a bank-linked Plaid account.
	SourcePlaid Source = "plaid"
)

// PaymentSource says how a loan's monthly payment was determined. Lunch Money
// has no "monthly payment" field, so every payment here is derived and the
// caller needs to know how much to trust it.
type PaymentSource string

const (
	// PaymentSourceAccountLink means a recurring expense pointed at this
	// account by ID. This is the trustworthy case.
	PaymentSourceAccountLink PaymentSource = "recurring_account_link"
	// PaymentSourcePayeeMatch means a recurring expense was matched to this
	// loan by payee name. Loan payments are usually booked against the
	// checking account they are paid from rather than the loan itself, so
	// this heuristic carries most of the matches -- and most of the risk.
	PaymentSourcePayeeMatch PaymentSource = "recurring_payee_match"
	// PaymentSourceOverride means the payment came from configuration.
	PaymentSourceOverride PaymentSource = "override"
	// PaymentSourceNone means nothing matched and monthly_payment is null.
	PaymentSourceNone PaymentSource = "none"
)

// loanTypes are the Lunch Money asset type_name / Plaid type values treated as
// loans by default. Credit cards are revolving debt rather than loans and are
// excluded unless explicitly requested.
var loanTypes = map[string]bool{"loan": true}

// liabilityTypes are the extra types folded in when liabilities are requested.
var liabilityTypes = map[string]bool{"other liability": true}

// Payment is a single recurring expense attributed to a loan.
type Payment struct {
	ID            int64   `json:"id"`
	Payee         string  `json:"payee"`
	Description   string  `json:"description,omitempty"`
	Amount        float64 `json:"amount"`
	Currency      string  `json:"currency"`
	Cadence       string  `json:"cadence"`
	MonthlyAmount float64 `json:"monthly_amount"`
	BillingDate   string  `json:"billing_date,omitempty"`
	MatchedBy     string  `json:"matched_by"`
}

// Loan is one debt with its balance and derived monthly payment.
//
// Balance is the amount owed, expressed positive, matching how Lunch Money
// reports loan balances. It is not negated into a net-worth sign convention.
type Loan struct {
	ID              string   `json:"id"`
	Source          Source   `json:"source"`
	AccountID       int64    `json:"account_id"`
	Name            string   `json:"name"`
	DisplayName     string   `json:"display_name,omitempty"`
	InstitutionName string   `json:"institution_name,omitempty"`
	Type            string   `json:"type"`
	Subtype         string   `json:"subtype,omitempty"`
	Status          string   `json:"status,omitempty"`
	Currency        string   `json:"currency"`
	Balance         float64  `json:"balance"`
	BalanceRaw      string   `json:"balance_raw"`
	BalanceAsOf     string   `json:"balance_as_of,omitempty"`
	MonthlyPayment  *float64 `json:"monthly_payment"`
	// PaymentSource is always set, so a null MonthlyPayment can be told apart
	// from a genuine zero.
	PaymentSource PaymentSource `json:"payment_source"`
	Payments      []Payment     `json:"payments,omitempty"`
}

// Totals aggregates the report. Balances in mixed currencies are summed only
// when every loan shares one currency; see Report.Notes.
type Totals struct {
	Count               int     `json:"count"`
	Balance             float64 `json:"balance"`
	MonthlyPayment      float64 `json:"monthly_payment"`
	LoansMissingPayment int     `json:"loans_missing_payment"`
}

// Report is the top-level JSON response.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Currency    string    `json:"currency,omitempty"`
	Totals      Totals    `json:"totals"`
	Loans       []Loan    `json:"loans"`
	Notes       []string  `json:"notes,omitempty"`
}

// Options controls which accounts count as loans.
type Options struct {
	// IncludeCredit folds in credit cards and other revolving credit.
	IncludeCredit bool
	// IncludeLiabilities folds in assets typed "other liability".
	IncludeLiabilities bool
	// Overrides maps a loan ID ("asset:12") to a monthly payment, for loans
	// whose payment cannot be derived from recurring expenses.
	Overrides map[string]float64
}

// wantType reports whether a Lunch Money type name counts as a loan.
func (o Options) wantType(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	if loanTypes[t] {
		return true
	}
	if o.IncludeLiabilities && liabilityTypes[t] {
		return true
	}
	if o.IncludeCredit && t == "credit" {
		return true
	}
	return false
}

// LoanFetcher is the subset of the Lunch Money client acntng needs. It exists
// so tests can supply canned data without an HTTP round trip.
type LoanFetcher interface {
	GetAssets(ctx context.Context) ([]*lunchmoney.Asset, error)
	GetPlaidAccounts(ctx context.Context) ([]*lunchmoney.PlaidAccount, error)
	GetRecurringExpenses(ctx context.Context, filters *lunchmoney.RecurringExpenseFilters) ([]*lunchmoney.RecurringExpense, error)
}

// BuildReport fetches accounts and recurring expenses and assembles the loan
// report.
//
// A failure to read recurring expenses is not fatal: balances are the primary
// answer, so the report degrades to null payments and records the reason in
// Notes rather than returning nothing.
func BuildReport(ctx context.Context, c LoanFetcher, now time.Time, opts Options) (*Report, error) {
	assets, err := c.GetAssets(ctx)
	if err != nil {
		return nil, fmt.Errorf("get assets: %w", err)
	}

	plaid, err := c.GetPlaidAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("get plaid accounts: %w", err)
	}

	report := &Report{GeneratedAt: now.UTC(), Loans: []Loan{}}

	// Ask for recurring expenses as of the first of the current month, which
	// is what the Lunch Money API expects to scope a recurring window.
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	recurring, err := c.GetRecurringExpenses(ctx, &lunchmoney.RecurringExpenseFilters{
		StartDate:       month,
		DebitAsNegative: false,
	})
	if err != nil {
		report.Notes = append(report.Notes,
			fmt.Sprintf("recurring expenses unavailable, monthly payments not derived: %v", err))
		recurring = nil
	}

	for _, a := range assets {
		if a == nil || !opts.wantType(a.TypeName) {
			continue
		}
		if isClosed(a.Status) {
			continue
		}
		report.Loans = append(report.Loans, loanFromAsset(a))
	}

	for _, p := range plaid {
		if p == nil || !opts.wantType(p.Type) {
			continue
		}
		if isClosed(p.Status) {
			continue
		}
		report.Loans = append(report.Loans, loanFromPlaid(p))
	}

	attachPayments(report.Loans, recurring, opts.Overrides)

	sort.Slice(report.Loans, func(i, j int) bool {
		if report.Loans[i].Balance != report.Loans[j].Balance {
			return report.Loans[i].Balance > report.Loans[j].Balance
		}
		return report.Loans[i].Name < report.Loans[j].Name
	})

	summarize(report)

	return report, nil
}

// isClosed reports whether an account status means the debt is settled. Lunch
// Money uses "closed" for assets and "inactive"/"relink" style values for
// Plaid; only a definitively closed account is dropped.
func isClosed(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "closed", "inactive":
		return true
	default:
		return false
	}
}

func loanFromAsset(a *lunchmoney.Asset) Loan {
	l := Loan{
		ID:              fmt.Sprintf("asset:%d", a.ID),
		Source:          SourceAsset,
		AccountID:       a.ID,
		Name:            a.Name,
		DisplayName:     a.DisplayName,
		InstitutionName: a.InstitutionName,
		Type:            a.TypeName,
		Subtype:         a.SubtypeName,
		Status:          a.Status,
		Currency:        strings.ToUpper(a.Currency),
		BalanceRaw:      a.Balance,
		Balance:         parseAmount(a.Balance),
		PaymentSource:   PaymentSourceNone,
	}
	if !a.BalanceAsOf.IsZero() {
		l.BalanceAsOf = a.BalanceAsOf.UTC().Format(time.RFC3339)
	}
	return l
}

func loanFromPlaid(p *lunchmoney.PlaidAccount) Loan {
	l := Loan{
		ID:              fmt.Sprintf("plaid:%d", p.ID),
		Source:          SourcePlaid,
		AccountID:       p.ID,
		Name:            p.Name,
		DisplayName:     p.DisplayName,
		InstitutionName: p.InstitutionName,
		Type:            p.Type,
		Subtype:         p.Subtype,
		Status:          p.Status,
		Currency:        strings.ToUpper(p.Currency),
		BalanceRaw:      p.Balance,
		Balance:         parseAmount(p.Balance),
		PaymentSource:   PaymentSourceNone,
	}
	if !p.BalanceLastUpdate.IsZero() {
		l.BalanceAsOf = p.BalanceLastUpdate.UTC().Format(time.RFC3339)
	}
	return l
}

// parseAmount reads a Lunch Money decimal string. The library's ParseCurrency
// helper is deliberately not used: it does int64(100*f), which truncates the
// cents of a value like "12345.679".
func parseAmount(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return round2(f)
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}

// monthlyFactor converts a Lunch Money cadence into a per-month multiplier.
// The bool is false for cadences that are not a recurring monthly obligation.
func monthlyFactor(cadence string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(cadence)) {
	case "weekly":
		return 52.0 / 12.0, true
	case "every 2 weeks", "biweekly":
		return 26.0 / 12.0, true
	case "twice a month", "semi-monthly":
		return 2, true
	case "monthly":
		return 1, true
	case "every 2 months":
		return 1.0 / 2.0, true
	case "every 3 months", "quarterly":
		return 1.0 / 3.0, true
	case "every 4 months":
		return 1.0 / 4.0, true
	case "twice a year", "semi-annually":
		return 1.0 / 6.0, true
	case "yearly", "annually":
		return 1.0 / 12.0, true
	default:
		// "once" and anything unrecognized are not a monthly obligation.
		return 0, false
	}
}

// attachPayments derives each loan's monthly payment from recurring expenses,
// then applies any configured overrides.
func attachPayments(loans []Loan, recurring []*lunchmoney.RecurringExpense, overrides map[string]float64) {
	// A recurring expense linked to an account by ID is authoritative, so
	// claim those first and keep them out of the fuzzier payee pass.
	claimed := map[int64]bool{}

	for i := range loans {
		l := &loans[i]
		for _, r := range recurring {
			if r == nil {
				continue
			}
			if !accountLinked(l, r) {
				continue
			}
			factor, ok := monthlyFactor(r.Cadence)
			if !ok {
				continue
			}
			l.Payments = append(l.Payments, payment(r, factor, string(PaymentSourceAccountLink)))
			claimed[r.ID] = true
		}
		if len(l.Payments) > 0 {
			l.PaymentSource = PaymentSourceAccountLink
		}
	}

	// Loan payments are commonly booked against the checking account they are
	// paid from rather than the loan, which leaves the ID link empty. Fall
	// back to matching the recurring payee against the loan's name.
	for i := range loans {
		l := &loans[i]
		if len(l.Payments) > 0 {
			continue
		}
		for _, r := range recurring {
			if r == nil || claimed[r.ID] {
				continue
			}
			if !payeeMatches(l, r) {
				continue
			}
			factor, ok := monthlyFactor(r.Cadence)
			if !ok {
				continue
			}
			l.Payments = append(l.Payments, payment(r, factor, string(PaymentSourcePayeeMatch)))
		}
		if len(l.Payments) > 0 {
			l.PaymentSource = PaymentSourcePayeeMatch
		}
	}

	for i := range loans {
		l := &loans[i]

		if amt, ok := overrides[l.ID]; ok {
			v := round2(amt)
			l.MonthlyPayment = &v
			l.PaymentSource = PaymentSourceOverride
			continue
		}

		if len(l.Payments) == 0 {
			l.PaymentSource = PaymentSourceNone
			continue
		}

		total := 0.0
		for _, p := range l.Payments {
			total += p.MonthlyAmount
		}
		v := round2(total)
		l.MonthlyPayment = &v
	}
}

func payment(r *lunchmoney.RecurringExpense, factor float64, matchedBy string) Payment {
	// Recurring expenses are requested with debit_as_negative=false, so a
	// payment is positive. Take the magnitude anyway so a sign flip upstream
	// cannot silently negate a total.
	amt := math.Abs(parseAmount(r.Amount))
	return Payment{
		ID:            r.ID,
		Payee:         r.Payee,
		Description:   r.Description,
		Amount:        amt,
		Currency:      strings.ToUpper(r.Currency),
		Cadence:       r.Cadence,
		MonthlyAmount: round2(amt * factor),
		BillingDate:   r.BillingDate,
		MatchedBy:     matchedBy,
	}
}

// accountLinked reports whether a recurring expense points at this loan by ID.
func accountLinked(l *Loan, r *lunchmoney.RecurringExpense) bool {
	switch l.Source {
	case SourceAsset:
		return r.AssetID != 0 && r.AssetID == l.AccountID
	case SourcePlaid:
		return r.PlaidAccountID != 0 && r.PlaidAccountID == l.AccountID
	default:
		return false
	}
}

// payeeMatches reports whether a recurring expense's payee looks like it pays
// this loan. It compares normalized names in both directions so "Sallie Mae"
// matches a "Sallie Mae Student Loan" payee and vice versa.
func payeeMatches(l *Loan, r *lunchmoney.RecurringExpense) bool {
	payee := normalize(r.Payee)
	if payee == "" {
		return false
	}

	for _, candidate := range []string{l.Name, l.DisplayName, l.InstitutionName} {
		name := normalize(candidate)
		// Short names produce nonsense substring hits ("bo" inside
		// "bookstore"), so require enough signal to be meaningful.
		if len(name) < 4 {
			continue
		}
		if strings.Contains(payee, name) || strings.Contains(name, payee) {
			return true
		}
	}

	return false
}

// normalize lowercases a name and strips everything but letters and digits so
// punctuation and spacing differences do not defeat the comparison.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// summarize fills in the report totals and flags anything that makes them
// misleading.
func summarize(rep *Report) {
	currencies := map[string]bool{}
	for _, l := range rep.Loans {
		rep.Totals.Count++
		rep.Totals.Balance += l.Balance
		if l.Currency != "" {
			currencies[l.Currency] = true
		}
		if l.MonthlyPayment == nil {
			rep.Totals.LoansMissingPayment++
			continue
		}
		rep.Totals.MonthlyPayment += *l.MonthlyPayment
	}

	rep.Totals.Balance = round2(rep.Totals.Balance)
	rep.Totals.MonthlyPayment = round2(rep.Totals.MonthlyPayment)

	switch len(currencies) {
	case 0:
	case 1:
		for c := range currencies {
			rep.Currency = c
		}
	default:
		list := make([]string, 0, len(currencies))
		for c := range currencies {
			list = append(list, c)
		}
		sort.Strings(list)
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"loans span multiple currencies (%s); totals are a raw sum and not converted",
			strings.Join(list, ", ")))
	}

	if rep.Totals.LoansMissingPayment > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d of %d loans have no derivable monthly payment; Lunch Money has no payment field, so a payment is only found when a recurring expense links to the account or its payee matches the loan name",
			rep.Totals.LoansMissingPayment, rep.Totals.Count))
	}
}
