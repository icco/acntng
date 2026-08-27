// Command acntng reports Lunch Money loan balances and monthly payments as JSON.
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

// Source says which Lunch Money collection a loan came from.
type Source string

const (
	SourceAsset Source = "asset"
	SourcePlaid Source = "plaid"
)

// PaymentSource says how a monthly payment was determined. Lunch Money has no
// such field, so callers need to know how much to trust it.
type PaymentSource string

const (
	// Linked by ID: reliable. Payee-matched: a heuristic.
	PaymentSourceAccountLink PaymentSource = "recurring_account_link"
	PaymentSourcePayeeMatch  PaymentSource = "recurring_payee_match"
	PaymentSourceOverride    PaymentSource = "override"
	PaymentSourceNone        PaymentSource = "none"
)

// Credit cards are revolving debt, so excluded unless asked for.
var loanTypes = map[string]bool{"loan": true}

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

// Loan is one debt. Balance is the amount owed, positive, as Lunch Money
// reports it -- not negated into a net-worth convention.
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
	// Always set, so a null MonthlyPayment is distinguishable from a real zero.
	PaymentSource PaymentSource `json:"payment_source"`
	Payments      []Payment     `json:"payments,omitempty"`
}

// Totals aggregates the report. Mixed currencies are an unconverted sum; see
// Report.Notes.
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
	IncludeCredit      bool
	IncludeLiabilities bool
	// Overrides maps a loan ID ("asset:12") to a monthly payment.
	Overrides map[string]float64
}

// wantType reports whether a type name counts as a loan.
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

// LoanFetcher is the slice of the Lunch Money client acntng needs, so tests can
// supply canned data.
type LoanFetcher interface {
	GetAssets(ctx context.Context) ([]*lunchmoney.Asset, error)
	GetPlaidAccounts(ctx context.Context) ([]*lunchmoney.PlaidAccount, error)
	GetRecurringExpenses(ctx context.Context, filters *lunchmoney.RecurringExpenseFilters) ([]*lunchmoney.RecurringExpense, error)
}

// BuildReport assembles the loan report. A recurring-expense failure is not
// fatal: balances are the primary answer, so payments degrade to null and the
// reason lands in Notes.
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

	// nil filters: the library's ToMap() always errors (icco/lunchmoney#24),
	// and the API defaults are what we want anyway.
	recurring, err := c.GetRecurringExpenses(ctx, nil)
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

	report.Notes = append(report.Notes, attachPayments(report.Loans, recurring, opts.Overrides)...)

	sort.Slice(report.Loans, func(i, j int) bool {
		if report.Loans[i].Balance != report.Loans[j].Balance {
			return report.Loans[i].Balance > report.Loans[j].Balance
		}
		return report.Loans[i].Name < report.Loans[j].Name
	})

	summarize(report)

	return report, nil
}

// isClosed reports whether the debt is settled.
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

// parseAmount reads a decimal string. Not ParseCurrency: it truncates cents.
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

// monthlyFactor converts a cadence to a per-month multiplier. False for
// anything that is not a recurring monthly obligation.
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
		// "once" and anything unrecognized.
		return 0, false
	}
}

// attachPayments derives monthly payments, applies overrides, and returns notes
// about anything it declined to guess at.
func attachPayments(loans []Loan, recurring []*lunchmoney.RecurringExpense, overrides map[string]float64) []string {
	var notes []string

	// ID-linked expenses are authoritative, so claim them before the payee pass.
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

	// Payments are usually booked against the account they are paid from, so
	// fall back to payee names. Driven by expense, not loan: one payee can
	// match two loans at an institution, and crediting both doubles the total.
	for _, r := range recurring {
		if r == nil || claimed[r.ID] {
			continue
		}
		if _, ok := monthlyFactor(r.Cadence); !ok {
			continue
		}

		best, bestScore, tied := -1, 0, false
		for i := range loans {
			if loans[i].PaymentSource == PaymentSourceAccountLink {
				continue
			}
			score := payeeMatchScore(&loans[i], r)
			switch {
			case score == 0:
			case score > bestScore:
				best, bestScore, tied = i, score, false
			case score == bestScore:
				tied = true
			}
		}

		if best < 0 {
			continue
		}
		if tied {
			notes = append(notes, fmt.Sprintf(
				"recurring payee %q matches more than one loan equally well; left unassigned rather than guessed",
				r.Payee))
			continue
		}

		factor, _ := monthlyFactor(r.Cadence)
		loans[best].Payments = append(loans[best].Payments,
			payment(r, factor, string(PaymentSourcePayeeMatch)))
		claimed[r.ID] = true
	}

	for i := range loans {
		l := &loans[i]
		if l.PaymentSource == PaymentSourceAccountLink {
			continue
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

	return notes
}

func payment(r *lunchmoney.RecurringExpense, factor float64, matchedBy string) Payment {
	// Magnitude, so a sign flip upstream cannot silently negate a total.
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

// accountLinked reports whether an expense points at this loan by ID.
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

// minMatchLen guards both directions: "Car" hits payee "Carwash", and payee
// "US" hits loan "Meridian US Loan".
const minMatchLen = 4

// payeeMatchScore returns the length of the longest loan name matching this
// payee, so callers can prefer the most specific loan. Zero means no match.
func payeeMatchScore(l *Loan, r *lunchmoney.RecurringExpense) int {
	payee := normalize(r.Payee)
	if len(payee) < minMatchLen {
		return 0
	}

	best := 0
	for _, candidate := range []string{l.Name, l.DisplayName, l.InstitutionName} {
		name := normalize(candidate)
		if len(name) < minMatchLen {
			continue
		}
		if strings.Contains(payee, name) || strings.Contains(name, payee) {
			if len(name) > best {
				best = len(name)
			}
		}
	}

	return best
}

// normalize strips case, punctuation and spacing before comparison.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// summarize fills in totals and flags anything that makes them misleading.
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
