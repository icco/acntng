package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/icco/lunchmoney"
)

// debtCategories splits debt service out of everyday spending. Lunch Money has
// no notion of "this category is a debt", so the split is by name.
var debtCategories = map[string]bool{
	"mortgage":             true,
	"student loans":        true,
	"auto loan":            true,
	"personal loans":       true,
	"buy now pay later":    true,
	"loan payments":        true,
	"credit card payments": true,
}

// isDebt reports whether a category name counts as debt service.
func isDebt(name string) bool {
	return debtCategories[strings.ToLower(strings.TrimSpace(name))]
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
	CategoryID int64   `json:"category_id"`
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
	Transactions int      `json:"transactions,omitempty"`
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
	DebtShare          *float64 `json:"debt_share"`
	CategoriesOver     int      `json:"categories_over"`
	UncategorizedSpent float64  `json:"uncategorized_spent,omitempty"`
	UncategorizedCount int64    `json:"uncategorized_count,omitempty"`
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
	GetBudgetSummary(ctx context.Context, filters *lunchmoney.BudgetFilters) (*lunchmoney.BudgetSummary, error)
	GetCategories(ctx context.Context, filters *lunchmoney.CategoryFilters) ([]*lunchmoney.Category, error)
}

// UserFetcher optionally retrieves user profile details like primary currency.
type UserFetcher interface {
	GetUser(ctx context.Context) (*lunchmoney.User, error)
}

// monthStart truncates to the first of the month, which is the only budget
// period start Lunch Money accepts for a monthly budget.
func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// BuildBudgetReport assembles the budget report for the month containing now.
func BuildBudgetReport(ctx context.Context, c BudgetFetcher, at time.Time) (*BudgetReport, error) {
	start := monthStart(at)
	end := start.AddDate(0, 1, -1)

	includeTotals := true
	summary, err := c.GetBudgetSummary(ctx, &lunchmoney.BudgetFilters{
		StartDate:     start.Format("2006-01-02"),
		EndDate:       end.Format("2006-01-02"),
		IncludeTotals: &includeTotals,
	})
	if err != nil {
		return nil, fmt.Errorf("get budget summary: %w", err)
	}

	categories, err := c.GetCategories(ctx, &lunchmoney.CategoryFilters{
		Format: lunchmoney.CategoryFormatFlattened,
	})
	if err != nil {
		return nil, fmt.Errorf("get categories: %w", err)
	}

	var primaryCurrency string
	if uf, ok := c.(UserFetcher); ok {
		if u, err := uf.GetUser(ctx); err == nil && u != nil && u.PrimaryCurrency != "" {
			primaryCurrency = strings.ToUpper(strings.TrimSpace(u.PrimaryCurrency))
		}
	}

	rep := &BudgetReport{
		GeneratedAt: at.UTC(),
		Month:       start.Format("2006-01"),
		Income:      []BudgetLine{},
		Debt:        []BudgetLine{},
		Living:      []BudgetLine{},
	}

	groupNames := make(map[int64]string)
	for _, cat := range categories {
		if cat != nil && cat.IsGroup {
			groupNames[cat.ID] = cat.Name
		}
	}

	summaryMap := summary.CategoryMap()
	skippedExcluded := 0

	for _, cat := range categories {
		if cat == nil || cat.IsGroup {
			continue
		}
		if cat.ExcludeFromBudget {
			skippedExcluded++
			continue
		}

		var groupName string
		if cat.GroupID != nil {
			groupName = groupNames[*cat.GroupID]
		}

		budgeted := 0.0
		spent := 0.0

		if sc := summaryMap[cat.ID]; sc != nil {
			if sc.Totals.Budgeted != nil {
				budgeted = *sc.Totals.Budgeted
			}
			activity := sc.Totals.OtherActivity + sc.Totals.RecurringActivity
			if cat.IsIncome {
				spent = math.Abs(activity)
			} else {
				spent = activity
			}
		}

		line := BudgetLine{
			CategoryID: cat.ID,
			Name:       cat.Name,
			GroupName:  groupName,
			IsIncome:   cat.IsIncome,
			IsDebt:     isDebt(cat.Name),
			Budgeted:   round2(budgeted),
			Spent:      round2(spent),
			Remaining:  round2(budgeted - spent),
		}
		if line.Budgeted > 0 {
			pct := round2(line.Spent / line.Budgeted * 100)
			line.PctUsed = &pct
		}

		// A category with neither a budget nor activity is noise.
		if line.Budgeted == 0 && line.Spent == 0 {
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

	if summary != nil && summary.Totals != nil {
		uncat := summary.Totals.Outflow.Uncategorized + summary.Totals.Outflow.UncategorizedRecurring
		if uncat > 0 {
			rep.Totals.UncategorizedSpent = round2(uncat)
			rep.Totals.UncategorizedCount = summary.Totals.Outflow.UncategorizedCount
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"%s was spent across %d uncategorized transactions this month",
				money(rep.Totals.UncategorizedSpent), rep.Totals.UncategorizedCount))
		}
	}

	summarizeBudget(rep, primaryCurrency)

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
func summarizeBudget(rep *BudgetReport, primaryCurrency string) {
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
	// Uncategorized outflow is spending and must be counted in OutflowSpent to keep ActualSurplus accurate.
	t.OutflowSpent = round2(t.DebtSpent + t.LivingSpent + t.UncategorizedSpent)
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

	if primaryCurrency != "" {
		rep.Currency = primaryCurrency
	} else if rep.Currency == "" {
		rep.Currency = "USD"
	}
}
