package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.icco.me/lunchmoney"
)

const (
	defaultBaselineMonths = 6
	monthKey              = "2006-01"
	dayKey                = "2006-01-02"
	transactionPageSize   = 2000
	uncategorizedName     = "Uncategorized"
	// maxMerchants caps the merchant table, which is context for a human and
	// prompt tokens for the model.
	maxMerchants = 12
)

// defaultPaycheckPattern matches the payroll processors and bank descriptors a
// direct deposit usually carries.
const defaultPaycheckPattern = `(?i)\b(payroll|paycheck|salary|gusto|adp|paychex|direct dep(osit)?)\b`

// defaultFixedCategories are bills that a month of trying harder does not
// move. Debt service is always fixed on top of these.
var defaultFixedCategories = []string{
	"rent", "utilities", "rent and utilities", "insurance", "medical", "taxes", "childcare",
}

// PlanOptions shapes a plan.
type PlanOptions struct {
	// Review is any instant inside the month being reviewed.
	Review time.Time
	// Now decides which month the plan is for.
	Now time.Time
	// BaselineMonths is how many months before the review month define normal.
	BaselineMonths int
	// PaycheckMatch picks paychecks out of income by payee.
	PaycheckMatch *regexp.Regexp
	// Fixed holds lowercased category names that are not worth trimming.
	Fixed map[string]bool
}

// PaycheckSource is one employer's deposits.
type PaycheckSource struct {
	Name string `json:"name"`
	// Typical is the median single deposit across the whole window.
	Typical float64 `json:"typical"`
	// MonthlyNormal is the median monthly total across the baseline.
	MonthlyNormal float64 `json:"monthly_normal"`
	ReviewCount   int     `json:"review_count"`
	ReviewTotal   float64 `json:"review_total"`
}

// CategoryLine compares one category's review month against its normal.
type CategoryLine struct {
	CategoryID int64  `json:"category_id"`
	Name       string `json:"name"`
	Fixed      bool   `json:"fixed"`
	// Normal is the median month across the baseline, Low the 25th percentile.
	Normal float64 `json:"normal"`
	Low    float64 `json:"low"`
	Review float64 `json:"review"`
	Delta  float64 `json:"delta"`
	// Target is what the plan asks of this category next month.
	Target       float64   `json:"target"`
	History      []float64 `json:"history"`
	Transactions int       `json:"transactions"`
}

// MerchantLine is one payee's flexible spending in the review month.
type MerchantLine struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Count    int     `json:"count"`
	Total    float64 `json:"total"`
}

// RecurringCharge is a flexible charge that lands once a month at a steady
// amount, found from the transactions themselves.
type RecurringCharge struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Monthly  float64 `json:"monthly"`
	Months   int     `json:"months"`
}

// PlanTotals are the headline figures. Fixed and flexible partition spend.
type PlanTotals struct {
	PaycheckNormal    float64 `json:"paycheck_normal"`
	PaycheckReview    float64 `json:"paycheck_review"`
	OtherIncomeNormal float64 `json:"other_income_normal"`
	OtherIncomeReview float64 `json:"other_income_review"`
	SpendNormal       float64 `json:"spend_normal"`
	SpendReview       float64 `json:"spend_review"`
	FixedNormal       float64 `json:"fixed_normal"`
	FixedReview       float64 `json:"fixed_review"`
	FlexibleNormal    float64 `json:"flexible_normal"`
	FlexibleReview    float64 `json:"flexible_review"`
	// MarginNormal is paychecks less spending in a normal month. Other income
	// is left out: a plan should not lean on money that may not arrive.
	MarginNormal float64 `json:"margin_normal"`
	MarginReview float64 `json:"margin_review"`
	// SavingsRate is MarginNormal over PaycheckNormal, nil without paychecks.
	SavingsRate      *float64 `json:"savings_rate"`
	TargetSpend      float64  `json:"target_spend"`
	TargetMargin     float64  `json:"target_margin"`
	PotentialSavings float64  `json:"potential_savings"`
}

// Plan is the top-level response: what a normal month looks like against the
// paychecks, how the review month differed, and what to change next month.
type Plan struct {
	GeneratedAt    time.Time         `json:"generated_at"`
	Month          string            `json:"month"`
	PlanFor        string            `json:"plan_for"`
	BaselineMonths []string          `json:"baseline_months"`
	Totals         PlanTotals        `json:"totals"`
	Paychecks      []PaycheckSource  `json:"paychecks"`
	Categories     []CategoryLine    `json:"categories"`
	Merchants      []MerchantLine    `json:"merchants"`
	Recurring      []RecurringCharge `json:"recurring"`
	Suggestions    []Suggestion      `json:"suggestions"`
	Advice         *Advice           `json:"advice,omitempty"`
	Notes          []string          `json:"notes,omitempty"`
}

// PrevMonth returns the review month before this one.
func (p *Plan) PrevMonth() string { return shiftMonth(p.Month, -1) }

// NextMonth returns the review month after this one.
func (p *Plan) NextMonth() string { return shiftMonth(p.Month, 1) }

func shiftMonth(month string, by int) string {
	t, err := time.Parse(monthKey, month)
	if err != nil {
		return ""
	}
	return t.AddDate(0, by, 0).Format(monthKey)
}

// TransactionFetcher is the slice of the Lunch Money client the plan needs.
type TransactionFetcher interface {
	GetTransactions(ctx context.Context, filters *lunchmoney.TransactionFilters) (*lunchmoney.TransactionsResponse, error)
	GetCategories(ctx context.Context, filters *lunchmoney.CategoryFilters) ([]*lunchmoney.Category, error)
}

// lastCompleteMonth is the default review month.
func lastCompleteMonth(now time.Time) time.Time {
	return monthStart(now).AddDate(0, -1, 0)
}

// planFor picks the month the advice applies to: the one after the review
// month, moved forward when that month is already past or mostly spent.
func planFor(review, now time.Time) time.Time {
	next := monthStart(review).AddDate(0, 1, 0)
	current := monthStart(now)
	if next.Before(current) {
		next = current
	}
	if next.Equal(current) && now.Day() > 15 {
		next = current.AddDate(0, 1, 0)
	}
	return next
}

// fixedSet builds the fixed-category lookup from a comma-separated override,
// falling back to the defaults when it is empty.
func fixedSet(override string) map[string]bool {
	names := defaultFixedCategories
	if strings.TrimSpace(override) != "" {
		names = strings.Split(override, ",")
	}
	out := map[string]bool{}
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out[n] = true
		}
	}
	return out
}

type merchantMonth struct {
	total float64
	count int
}

type merchantAgg struct {
	name     string
	category string
	months   map[string]*merchantMonth
}

type sourceAgg struct {
	name     string
	deposits []float64
	months   map[string]*merchantMonth
}

// ledger is the window's transactions rolled up by month.
type ledger struct {
	spend     map[int64]map[string]float64
	counts    map[int64]map[string]int
	paycheck  map[string]float64
	other     map[string]float64
	active    map[string]bool
	merchants map[string]*merchantAgg
	sources   map[string]*sourceAgg

	skippedCredits int
}

func newLedger() *ledger {
	return &ledger{
		spend:     map[int64]map[string]float64{},
		counts:    map[int64]map[string]int{},
		paycheck:  map[string]float64{},
		other:     map[string]float64{},
		active:    map[string]bool{},
		merchants: map[string]*merchantAgg{},
		sources:   map[string]*sourceAgg{},
	}
}

// BuildPlan assembles the plan for the month containing opts.Review.
func BuildPlan(ctx context.Context, c TransactionFetcher, opts PlanOptions) (*Plan, error) {
	if opts.BaselineMonths <= 0 {
		opts.BaselineMonths = defaultBaselineMonths
	}
	if opts.PaycheckMatch == nil {
		opts.PaycheckMatch = regexp.MustCompile(defaultPaycheckPattern)
	}
	if opts.Fixed == nil {
		opts.Fixed = fixedSet("")
	}

	review := monthStart(opts.Review)
	start := review.AddDate(0, -opts.BaselineMonths, 0)
	end := review.AddDate(0, 1, -1)

	categories, err := c.GetCategories(ctx, &lunchmoney.CategoryFilters{
		Format: lunchmoney.CategoryFormatFlattened,
	})
	if err != nil {
		return nil, fmt.Errorf("get categories: %w", err)
	}
	byID := map[int64]*lunchmoney.Category{}
	for _, cat := range categories {
		if cat != nil {
			byID[cat.ID] = cat
		}
	}

	txs, err := fetchTransactions(ctx, c, start, end)
	if err != nil {
		return nil, err
	}

	isFixed := func(name string) bool {
		return isDebt(name) || opts.Fixed[strings.ToLower(strings.TrimSpace(name))]
	}

	l := newLedger()
	for _, tx := range txs {
		l.add(tx, byID, opts.PaycheckMatch, isFixed)
	}

	reviewKey := review.Format(monthKey)
	var baseline []string
	for m := start; m.Before(review); m = m.AddDate(0, 1, 0) {
		key := m.Format(monthKey)
		// Months before the account's history begins would drag every median
		// toward zero, so the baseline starts at the first month with data.
		if len(baseline) == 0 && !l.active[key] {
			continue
		}
		baseline = append(baseline, key)
	}

	p := &Plan{
		GeneratedAt:    opts.Now.UTC(),
		Month:          reviewKey,
		PlanFor:        planFor(review, opts.Now).Format(monthKey),
		BaselineMonths: baseline,
		Paychecks:      []PaycheckSource{},
		Categories:     []CategoryLine{},
		Merchants:      []MerchantLine{},
		Recurring:      []RecurringCharge{},
		Suggestions:    []Suggestion{},
	}
	if baseline == nil {
		p.BaselineMonths = []string{}
	}

	p.Categories = l.categoryLines(byID, baseline, reviewKey, isFixed)
	p.Paychecks = l.paycheckSources(baseline, reviewKey)
	p.Merchants = l.reviewMerchants(reviewKey)
	p.Recurring = l.recurringCharges(baseline, reviewKey)

	t := &p.Totals
	t.PaycheckNormal = round2(median(series(l.paycheck, baseline)))
	t.PaycheckReview = round2(l.paycheck[reviewKey])
	t.OtherIncomeNormal = round2(median(series(l.other, baseline)))
	t.OtherIncomeReview = round2(l.other[reviewKey])

	suggest(p)

	for _, line := range p.Categories {
		t.SpendNormal += line.Normal
		t.SpendReview += line.Review
		t.TargetSpend += line.Target
		if line.Fixed {
			t.FixedNormal += line.Normal
			t.FixedReview += line.Review
		} else {
			t.FlexibleNormal += line.Normal
			t.FlexibleReview += line.Review
		}
	}
	for _, f := range []*float64{
		&t.SpendNormal, &t.SpendReview, &t.TargetSpend,
		&t.FixedNormal, &t.FixedReview, &t.FlexibleNormal, &t.FlexibleReview,
	} {
		*f = round2(*f)
	}
	t.MarginNormal = round2(t.PaycheckNormal - t.SpendNormal)
	t.MarginReview = round2(t.PaycheckReview - t.SpendReview)
	t.TargetMargin = round2(t.PaycheckNormal - t.TargetSpend)
	if t.PaycheckNormal > 0 {
		rate := round2(t.MarginNormal / t.PaycheckNormal)
		t.SavingsRate = &rate
	}

	headline(p)
	p.notes(l, opts.BaselineMonths)

	return p, nil
}

// fetchTransactions pages through every transaction in the window.
func fetchTransactions(ctx context.Context, c TransactionFetcher, start, end time.Time) ([]*lunchmoney.Transaction, error) {
	var out []*lunchmoney.Transaction
	for {
		resp, err := c.GetTransactions(ctx, &lunchmoney.TransactionFilters{
			StartDate: new(start.Format(dayKey)),
			EndDate:   new(end.Format(dayKey)),
			Limit:     new(int64(transactionPageSize)),
			Offset:    new(int64(len(out))),
		})
		if err != nil {
			return nil, fmt.Errorf("get transactions: %w", err)
		}
		if resp == nil || len(resp.Transactions) == 0 {
			return out, nil
		}
		out = append(out, resp.Transactions...)
		if !resp.HasMore {
			return out, nil
		}
	}
}

func (l *ledger) add(tx *lunchmoney.Transaction, byID map[int64]*lunchmoney.Category, paycheck *regexp.Regexp, isFixed func(string) bool) {
	if tx == nil || tx.IsPending || tx.IsSplitParent || tx.GroupParentID != nil || len(tx.Date) < 7 {
		return
	}
	month := tx.Date[:7]

	var cat *lunchmoney.Category
	if tx.CategoryID != nil {
		cat = byID[*tx.CategoryID]
	}
	// Transfers and card payments settle spending already counted elsewhere.
	if cat != nil && (cat.ExcludeFromTotals || cat.ExcludeFromBudget) {
		return
	}

	if cat != nil && cat.IsIncome {
		l.active[month] = true
		amount := -tx.ToBase
		if amount > 0 && (paycheck.MatchString(tx.Payee) || paycheck.MatchString(tx.OriginalName)) {
			l.paycheck[month] += amount
			l.addSource(tx.Payee, month, amount)
			return
		}
		l.other[month] += amount
		return
	}

	var id int64
	name := uncategorizedName
	if cat != nil {
		id, name = cat.ID, cat.Name
	} else if tx.ToBase < 0 {
		// An uncategorized credit is as likely a transfer as a refund.
		l.skippedCredits++
		return
	}

	l.active[month] = true
	if l.spend[id] == nil {
		l.spend[id] = map[string]float64{}
		l.counts[id] = map[string]int{}
	}
	l.spend[id][month] += tx.ToBase
	l.counts[id][month]++

	if isFixed(name) || tx.ToBase <= 0 || strings.TrimSpace(tx.Payee) == "" {
		return
	}
	key := strings.ToLower(strings.TrimSpace(tx.Payee))
	m := l.merchants[key]
	if m == nil {
		m = &merchantAgg{name: strings.TrimSpace(tx.Payee), category: name, months: map[string]*merchantMonth{}}
		l.merchants[key] = m
	}
	bump(m.months, month, tx.ToBase)
}

func bump(months map[string]*merchantMonth, month string, amount float64) {
	mm := months[month]
	if mm == nil {
		mm = &merchantMonth{}
		months[month] = mm
	}
	mm.total += amount
	mm.count++
}

func (l *ledger) addSource(payee, month string, amount float64) {
	name := sourceName(payee)
	s := l.sources[name]
	if s == nil {
		s = &sourceAgg{name: name, months: map[string]*merchantMonth{}}
		l.sources[name] = s
	}
	s.deposits = append(s.deposits, amount)
	bump(s.months, month, amount)
}

// sourceName reduces a bank descriptor to an employer: the trace numbers that
// change every pay period go, and so does the trailing recipient name.
func sourceName(payee string) string {
	var kept []string
	for word := range strings.FieldsSeq(payee) {
		if strings.ContainsAny(word, "0123456789") {
			continue
		}
		kept = append(kept, word)
		if len(kept) == 3 {
			break
		}
	}
	if len(kept) == 0 {
		return "Unknown"
	}
	return strings.Join(kept, " ")
}

func (l *ledger) categoryLines(byID map[int64]*lunchmoney.Category, baseline []string, reviewKey string, isFixed func(string) bool) []CategoryLine {
	lines := []CategoryLine{}
	for id, months := range l.spend {
		name := uncategorizedName
		if cat := byID[id]; cat != nil {
			name = cat.Name
		}
		history := series(months, baseline)
		line := CategoryLine{
			CategoryID:   id,
			Name:         name,
			Fixed:        isFixed(name),
			Normal:       round2(median(history)),
			Low:          round2(quantile(history, 0.25)),
			Review:       round2(months[reviewKey]),
			History:      history,
			Transactions: l.counts[id][reviewKey],
		}
		// Refunds can outweigh a quiet category's spending; a negative
		// "normal" would read as income.
		line.Normal = max(line.Normal, 0)
		line.Low = max(line.Low, 0)
		line.Delta = round2(line.Review - line.Normal)
		line.Target = line.Normal
		if line.Normal == 0 && line.Review == 0 {
			continue
		}
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool {
		a, b := max(lines[i].Normal, lines[i].Review), max(lines[j].Normal, lines[j].Review)
		if a != b {
			return a > b
		}
		return lines[i].Name < lines[j].Name
	})
	return lines
}

func (l *ledger) paycheckSources(baseline []string, reviewKey string) []PaycheckSource {
	out := []PaycheckSource{}
	for _, s := range l.sources {
		monthly := make([]float64, 0, len(baseline))
		for _, m := range baseline {
			if mm := s.months[m]; mm != nil {
				monthly = append(monthly, mm.total)
			} else {
				monthly = append(monthly, 0)
			}
		}
		src := PaycheckSource{
			Name:          s.name,
			Typical:       round2(median(s.deposits)),
			MonthlyNormal: round2(median(monthly)),
		}
		if mm := s.months[reviewKey]; mm != nil {
			src.ReviewCount, src.ReviewTotal = mm.count, round2(mm.total)
		}
		out = append(out, src)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MonthlyNormal != out[j].MonthlyNormal {
			return out[i].MonthlyNormal > out[j].MonthlyNormal
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (l *ledger) reviewMerchants(reviewKey string) []MerchantLine {
	out := []MerchantLine{}
	for _, m := range l.merchants {
		mm := m.months[reviewKey]
		if mm == nil {
			continue
		}
		out = append(out, MerchantLine{Name: m.name, Category: m.category, Count: mm.count, Total: round2(mm.total)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > maxMerchants {
		out = out[:maxMerchants]
	}
	return out
}

// recurringCharges finds subscriptions without relying on anyone having
// registered them: one charge a month, at a steady amount, still running.
func (l *ledger) recurringCharges(baseline []string, reviewKey string) []RecurringCharge {
	window := append(append([]string{}, baseline...), reviewKey)
	out := []RecurringCharge{}
	for _, m := range l.merchants {
		latest := m.months[reviewKey]
		if latest == nil || latest.count != 1 {
			continue
		}
		seen, lo, hi := 0, latest.total, latest.total
		steady := true
		for _, key := range window {
			mm := m.months[key]
			if mm == nil {
				continue
			}
			if mm.count != 1 {
				steady = false
				break
			}
			seen++
			lo, hi = min(lo, mm.total), max(hi, mm.total)
		}
		if !steady || seen < 3 || hi > lo*1.25 {
			continue
		}
		out = append(out, RecurringCharge{Name: m.name, Category: m.category, Monthly: round2(latest.total), Months: seen})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Monthly != out[j].Monthly {
			return out[i].Monthly > out[j].Monthly
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (p *Plan) notes(l *ledger, wanted int) {
	switch n := len(p.BaselineMonths); {
	case n == 0:
		p.Notes = append(p.Notes, "no transactions before "+p.Month+", so there is no normal month to compare against yet")
	case n < wanted:
		p.Notes = append(p.Notes, fmt.Sprintf(
			"normal is drawn from %d months rather than %d, because the transaction history starts in %s",
			n, wanted, p.BaselineMonths[0]))
	}
	if p.Totals.PaycheckNormal == 0 && p.Totals.OtherIncomeNormal > 0 {
		p.Notes = append(p.Notes, "no income matched the paycheck pattern; set ACNTNG_PAYCHECK_MATCH to a regular expression that matches the payroll payee")
	}
	if l.skippedCredits > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf(
			"%d uncategorized credits were left out, since an unlabelled deposit is as likely a transfer as a refund",
			l.skippedCredits))
	}
	p.Notes = append(p.Notes,
		"normal is each category's median month, so the total is a sum of medians rather than any one month; transfers and card payments are excluded")
}

// series reads months out of a map in order, with zero for a missing month.
func series(by map[string]float64, months []string) []float64 {
	out := make([]float64, 0, len(months))
	for _, m := range months {
		out = append(out, round2(by[m]))
	}
	return out
}

func median(v []float64) float64 { return quantile(v, 0.5) }

// quantile interpolates linearly between the two nearest ranks.
func quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64{}, v...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	lo := int(pos)
	if lo >= len(s)-1 {
		return s[len(s)-1]
	}
	return s[lo] + (s[lo+1]-s[lo])*(pos-float64(lo))
}
