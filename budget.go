package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/icco/lunchmoney"
)

// defaultDebtCategories splits debt service out of everyday spending. Lunch
// Money has no notion of "this category is a debt", so the split is by name.
// Override with ACNTNG_DEBT_CATEGORIES.
var defaultDebtCategories = []string{
	"mortgage",
	"student loans",
	"auto loan",
	"personal loans",
	"buy now pay later",
	"loan payments",
	"credit card payments",
}

// IncomeBasis says which income figure the surplus was computed against.
// Lunch Money derives income from deposits as they land, so early in a month
// the actual figure understates a normal month.
type IncomeBasis string

const (
	IncomeBasisBudgeted IncomeBasis = "budgeted"
	IncomeBasisActual   IncomeBasis = "actual"
)

// BudgetLine is one category's budget for a single month. Spent is positive
// for outflow, matching how Lunch Money reports it.
type BudgetLine struct {
	CategoryID int     `json:"category_id"`
	Name       string  `json:"name"`
	GroupName  string  `json:"group_name,omitempty"`
	IsIncome   bool    `json:"is_income"`
	IsDebt     bool    `json:"is_debt"`
	Budgeted   float64 `json:"budgeted"`
	Spent      float64 `json:"spent"`
	Remaining  float64 `json:"remaining"`
	// PctUsed is nil when nothing is budgeted, so "0% used" and "no budget
	// set" stay distinguishable.
	PctUsed      *float64 `json:"pct_used"`
	Transactions int      `json:"transactions"`
}

// Over reports whether spending has passed the budget for this line.
func (l BudgetLine) Over() bool {
	return l.Budgeted > 0 && l.Spent > l.Budgeted
}

// BudgetTotals aggregates a month. Debt and living partition outflow.
type BudgetTotals struct {
	IncomeBudgeted  float64     `json:"income_budgeted"`
	IncomeActual    float64     `json:"income_actual"`
	IncomeBasis     IncomeBasis `json:"income_basis"`
	DebtBudgeted    float64     `json:"debt_budgeted"`
	DebtSpent       float64     `json:"debt_spent"`
	LivingBudgeted  float64     `json:"living_budgeted"`
	LivingSpent     float64     `json:"living_spent"`
	OutflowBudgeted float64     `json:"outflow_budgeted"`
	OutflowSpent    float64     `json:"outflow_spent"`
	// PlannedSurplus is income less everything budgeted: what the month is
	// designed to save. ActualSurplus is the same against money actually spent.
	PlannedSurplus float64 `json:"planned_surplus"`
	ActualSurplus  float64 `json:"actual_surplus"`
	// DebtShare is debt service as a fraction of income, nil without income.
	DebtShare      *float64 `json:"debt_share"`
	CategoriesOver int      `json:"categories_over"`
}

// BudgetReport is the top-level budget response for one month.
type BudgetReport struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Month       string       `json:"month"`
	Currency    string       `json:"currency,omitempty"`
	Totals      BudgetTotals `json:"totals"`
	Income      []BudgetLine `json:"income"`
	Debt        []BudgetLine `json:"debt"`
	Living      []BudgetLine `json:"living"`
	Notes       []string     `json:"notes,omitempty"`
}

// PrevMonth and NextMonth are the adjacent budget periods, so a view layer can
// offer navigation without repeating the date arithmetic.
func (r *BudgetReport) PrevMonth() string { return r.shift(-1) }

// NextMonth returns the following budget period.
func (r *BudgetReport) NextMonth() string { return r.shift(1) }

func (r *BudgetReport) shift(months int) string {
	t, err := time.Parse("2006-01", r.Month)
	if err != nil {
		return ""
	}
	return t.AddDate(0, months, 0).Format("2006-01")
}

// BudgetFetcher is the slice of the Lunch Money client the budget report needs.
type BudgetFetcher interface {
	GetBudgets(ctx context.Context, filters *lunchmoney.BudgetFilters) ([]*lunchmoney.Budget, error)
}

// parseDebtCategories reads ACNTNG_DEBT_CATEGORIES: a comma-separated list of
// category names to count as debt service. Empty falls back to the defaults.
func parseDebtCategories(s string) map[string]bool {
	names := defaultDebtCategories
	if strings.TrimSpace(s) != "" {
		names = strings.Split(s, ",")
	}

	out := map[string]bool{}
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out[n] = true
		}
	}
	return out
}

// monthStart truncates to the first of the month, which is the only budget
// period start Lunch Money accepts for a monthly budget.
func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// BuildBudgetReport assembles the budget report for the month containing now.
//
// A nil debt map falls back to the defaults: a caller that forgot to set it
// would otherwise file every loan payment under everyday living, which is the
// one distinction this report exists to make. An empty-but-non-nil map is a
// deliberate "classify nothing as debt" and is honoured.
func BuildBudgetReport(ctx context.Context, c BudgetFetcher, at time.Time, debt map[string]bool) (*BudgetReport, error) {
	if debt == nil {
		debt = parseDebtCategories("")
	}

	start := monthStart(at)
	end := start.AddDate(0, 1, -1)
	month := start.Format("2006-01-02")

	budgets, err := c.GetBudgets(ctx, &lunchmoney.BudgetFilters{
		StartDate: month,
		EndDate:   end.Format("2006-01-02"),
	})
	if err != nil {
		return nil, fmt.Errorf("get budgets: %w", err)
	}

	rep := &BudgetReport{
		GeneratedAt: at.UTC(),
		Month:       start.Format("2006-01"),
		Income:      []BudgetLine{},
		Debt:        []BudgetLine{},
		Living:      []BudgetLine{},
	}

	currencies := map[string]bool{}
	skippedExcluded := 0

	for _, b := range budgets {
		if b == nil || b.IsGroup {
			// Groups restate their children; counting both doubles the total.
			continue
		}
		if b.ExcludeFromBudget {
			// Transfers and card payments. Real cash movement, but not
			// spending, and including them double-counts every purchase.
			skippedExcluded++
			continue
		}

		data, ok := b.Data[month]
		if !ok || data == nil {
			continue
		}
		if data.BudgetCurrency != "" {
			currencies[strings.ToUpper(data.BudgetCurrency)] = true
		}

		line := lineFrom(b, data, debt)

		// A category with neither a budget nor activity is noise.
		if line.Budgeted == 0 && line.Spent == 0 && line.Transactions == 0 {
			continue
		}

		switch {
		case line.IsIncome:
			rep.Income = append(rep.Income, line)
		case line.IsDebt:
			rep.Debt = append(rep.Debt, line)
		default:
			rep.Living = append(rep.Living, line)
		}
	}

	for _, set := range [][]BudgetLine{rep.Income, rep.Debt, rep.Living} {
		sortLines(set)
	}

	summarizeBudget(rep, currencies)

	if skippedExcluded > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d categories flagged 'exclude from budgets' were left out; these are transfers and credit-card payments, which are settlement rather than spending",
			skippedExcluded))
	}
	switch {
	case rep.Totals.IncomeBudgeted == 0 && rep.Totals.IncomeActual == 0:
		// Nothing to measure against: a "surplus" here is just the negated
		// outflow, which reads as a catastrophe on any future month.
		rep.Notes = append(rep.Notes,
			"no income is budgeted or received for this month, so the planned surplus is only the negated outflow and means nothing; set a budget on an income category to make it meaningful")
	case rep.Totals.IncomeBasis == IncomeBasisActual:
		rep.Notes = append(rep.Notes,
			"no income budget is set, so the surplus is measured against income received so far this month; early in the month that understates a normal month")
	}

	return rep, nil
}

// lineFrom converts one Lunch Money budget row into a report line.
func lineFrom(b *lunchmoney.Budget, data *lunchmoney.BudgetData, debt map[string]bool) BudgetLine {
	line := BudgetLine{
		CategoryID:   b.CategoryID,
		Name:         b.CategoryName,
		GroupName:    b.CategoryGroupName,
		IsIncome:     b.IsIncome,
		IsDebt:       debt[strings.ToLower(strings.TrimSpace(b.CategoryName))],
		Budgeted:     round2(budgetAmount(data)),
		Transactions: data.NumTransactions,
	}

	// Income arrives as a credit, so magnitude keeps a sign convention change
	// upstream from silently negating the total.
	if line.IsIncome {
		line.Spent = round2(math.Abs(data.SpendingToBase))
	} else {
		line.Spent = round2(data.SpendingToBase)
	}

	line.Remaining = round2(line.Budgeted - line.Spent)
	if line.Budgeted > 0 {
		pct := round2(line.Spent / line.Budgeted * 100)
		line.PctUsed = &pct
	}

	return line
}

// budgetAmount prefers the base-currency figure and falls back to the raw
// amount, which is what a single-currency account populates.
func budgetAmount(data *lunchmoney.BudgetData) float64 {
	if data.BudgetToBase != 0 {
		return data.BudgetToBase
	}
	if f, err := data.BudgetAmount.Float64(); err == nil {
		return f
	}
	return 0
}

// sortLines orders by budget then name, so the biggest commitments lead and
// the order is stable between requests.
func sortLines(lines []BudgetLine) {
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].Budgeted != lines[j].Budgeted {
			return lines[i].Budgeted > lines[j].Budgeted
		}
		if lines[i].Spent != lines[j].Spent {
			return lines[i].Spent > lines[j].Spent
		}
		return lines[i].Name < lines[j].Name
	})
}

// summarizeBudget fills in totals and flags what makes them misleading.
func summarizeBudget(rep *BudgetReport, currencies map[string]bool) {
	t := &rep.Totals

	for _, l := range rep.Income {
		t.IncomeBudgeted += l.Budgeted
		t.IncomeActual += l.Spent
	}
	for _, l := range rep.Debt {
		t.DebtBudgeted += l.Budgeted
		t.DebtSpent += l.Spent
		if l.Over() {
			t.CategoriesOver++
		}
	}
	for _, l := range rep.Living {
		t.LivingBudgeted += l.Budgeted
		t.LivingSpent += l.Spent
		if l.Over() {
			t.CategoriesOver++
		}
	}

	t.OutflowBudgeted = round2(t.DebtBudgeted + t.LivingBudgeted)
	t.OutflowSpent = round2(t.DebtSpent + t.LivingSpent)
	t.IncomeBudgeted = round2(t.IncomeBudgeted)
	t.IncomeActual = round2(t.IncomeActual)
	t.DebtBudgeted = round2(t.DebtBudgeted)
	t.DebtSpent = round2(t.DebtSpent)
	t.LivingBudgeted = round2(t.LivingBudgeted)
	t.LivingSpent = round2(t.LivingSpent)

	// Lunch Money accounts often budget no income at all, because income is
	// observed rather than planned. Fall back so the surplus stays meaningful.
	income := t.IncomeBudgeted
	t.IncomeBasis = IncomeBasisBudgeted
	if income == 0 {
		income = t.IncomeActual
		t.IncomeBasis = IncomeBasisActual
	}

	t.PlannedSurplus = round2(income - t.OutflowBudgeted)
	t.ActualSurplus = round2(t.IncomeActual - t.OutflowSpent)

	if income > 0 {
		share := round2(t.DebtBudgeted / income)
		t.DebtShare = &share
	}

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
			"budgets span multiple currencies (%s); totals are a raw sum and not converted",
			strings.Join(list, ", ")))
	}
}

// debtCategoriesFromEnv is the process-wide debt classification.
func debtCategoriesFromEnv() map[string]bool {
	return parseDebtCategories(os.Getenv("ACNTNG_DEBT_CATEGORIES"))
}
