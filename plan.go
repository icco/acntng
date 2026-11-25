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
	defaultHistoryMonths = 12
	defaultSavingsTarget = 1000.0
	// latelyFallbackMonths is how many complete months count as lately when
	// no ACNTNG_SINCE is set.
	latelyFallbackMonths = 3
	monthKey             = "2006-01"
	dayKey               = "2006-01-02"
	transactionPageSize  = 2000
	uncategorizedName    = "Uncategorized"
	feesName             = "Fees and interest"

	// spikeRatio marks a charge this many times a payee's usual one as a
	// payoff or a deposit rather than the bill itself.
	spikeRatio = 3.0
	// billLookbackDays is how far back a bill must have charged to be due.
	billLookbackDays = 92
	// stoppedAfterDays is how long a bill can go quiet before it is treated
	// as stopped.
	stoppedAfterDays = 45
	// recurringMinMonths is how many separate months a payee must appear in
	// to be a bill rather than an occasional cost.
	recurringMinMonths = 2
	// occasionalMaxMonths is the most months an occasional payee appears in
	// across the whole history.
	occasionalMaxMonths = 2
	oneOffMin           = 500.0
	// payoffMin is the smallest debt payment read as a payoff.
	payoffMin  = 5000.0
	maxOneOffs = 12
	// partialMonthMinDay is the day from which the month in progress counts
	// toward lately, scaled up to a full month.
	partialMonthMinDay = 15
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
	// Now decides the month planned for, which is always the next one.
	Now time.Time
	// HistoryMonths is how many complete months of history are read.
	HistoryMonths int
	// Since is the first month of the current way of living. Bills and
	// recent spending are judged from it. Zero means the last three months.
	Since time.Time
	// SavingsTarget is what the plan aims to leave over each month.
	SavingsTarget float64
	// PaycheckMatch picks paychecks out of income by payee.
	PaycheckMatch *regexp.Regexp
	// Fixed holds lowercased category names that are bills, not choices.
	Fixed map[string]bool
}

// IncomeLine is one employer's expected pay next month.
type IncomeLine struct {
	Name    string  `json:"name"`
	Monthly float64 `json:"monthly"`
}

// BillLine is a cost next month that is owed rather than chosen.
type BillLine struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Monthly  float64 `json:"monthly"`
	// Plan is what the plan budgets for it: Monthly, or zero once cancelled.
	Plan float64 `json:"plan"`
	Debt bool    `json:"debt"`
	// FirstSeen is the first month the payee charged in the history read.
	FirstSeen string `json:"first_seen"`
	// Occasional marks a category's irregular costs, budgeted at their usual
	// month rather than tied to one payee.
	Occasional bool `json:"occasional,omitempty"`
}

// StoppedBill is a bill that charged recently and is no longer due.
type StoppedBill struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Monthly  float64 `json:"monthly"`
	Reason   string  `json:"reason"`
}

// CapLine is one category of chosen spending: what it usually costs, what it
// has cost lately, and what the plan allows next month.
type CapLine struct {
	Category string  `json:"category"`
	Usual    float64 `json:"usual"`
	Lately   float64 `json:"lately"`
	Cap      float64 `json:"cap"`
	// Low is the 25th percentile month, the floor a cut aims for.
	Low float64 `json:"low"`
	// History is the category's complete months, oldest first.
	History []float64 `json:"history"`
}

// OneOff is a large charge read but not planned for again.
type OneOff struct {
	Date     string  `json:"date"`
	Payee    string  `json:"payee"`
	Category string  `json:"category"`
	Amount   float64 `json:"amount"`
	Reason   string  `json:"reason"`
}

// PlanTotals are the headline figures for the month planned.
type PlanTotals struct {
	Income   float64 `json:"income"`
	Bills    float64 `json:"bills"`
	Spending float64 `json:"spending"`
	// Unchanged is what the month leaves if nothing changes.
	Unchanged float64 `json:"unchanged"`
	Savings   float64 `json:"savings"`
	// Planned is what the month leaves with every change made.
	Planned float64 `json:"planned"`
	Target  float64 `json:"target"`
	// Short is how far Planned falls below Target, zero when it does not.
	Short float64 `json:"short"`
}

// Plan says what to change next month, from every month of history read.
type Plan struct {
	GeneratedAt time.Time `json:"generated_at"`
	PlanFor     string    `json:"plan_for"`
	// Months is the complete history read; Lately the months that describe
	// how things are now.
	Months  []string      `json:"months"`
	Lately  []string      `json:"lately"`
	Totals  PlanTotals    `json:"totals"`
	Changes []Change      `json:"changes"`
	Income  []IncomeLine  `json:"income"`
	Bills   []BillLine    `json:"bills"`
	Caps    []CapLine     `json:"caps"`
	Stopped []StoppedBill `json:"stopped"`
	OneOffs []OneOff      `json:"one_offs"`
	Notes   []string      `json:"notes,omitempty"`
	// Options are every lever the changes were chosen from.
	Options []Option `json:"options"`
	// ChosenBy is "rules", or the model that ranked the options.
	ChosenBy string `json:"chosen_by"`
	// Summary is the model's two sentences on the month, when it ranked.
	Summary string `json:"summary,omitempty"`
}

// TransactionFetcher is the slice of the Lunch Money client the plan reads
// transactions through.
type TransactionFetcher interface {
	GetTransactions(ctx context.Context, filters *lunchmoney.TransactionFilters) (*lunchmoney.TransactionsResponse, error)
	GetCategories(ctx context.Context, filters *lunchmoney.CategoryFilters) ([]*lunchmoney.Category, error)
}

// PlanFetcher is everything the plan reads: transactions, and the loan
// balances that say whether a debt is paid off.
type PlanFetcher interface {
	TransactionFetcher
	LoanFetcher
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

// charge is one spending transaction, reduced to what the plan uses.
type charge struct {
	date     time.Time
	month    string
	payee    string
	key      string
	category string
	amount   float64
	fixed    bool
	debt     bool
	fee      string
	// oneOff marks a large purchase that is not planned for again.
	oneOff bool
}

// deposit is one paycheck.
type deposit struct {
	date   time.Time
	source string
	amount float64
}

// BuildPlan reads the history and plans the month after opts.Now.
func BuildPlan(ctx context.Context, c PlanFetcher, opts PlanOptions) (*Plan, error) {
	if opts.HistoryMonths <= 0 {
		opts.HistoryMonths = defaultHistoryMonths
	}
	if opts.PaycheckMatch == nil {
		opts.PaycheckMatch = regexp.MustCompile(defaultPaycheckPattern)
	}
	if opts.Fixed == nil {
		opts.Fixed = fixedSet("")
	}

	now := opts.Now
	current := monthStart(now)
	start := current.AddDate(0, -opts.HistoryMonths, 0)

	categories, err := c.GetCategories(ctx, &lunchmoney.CategoryFilters{Format: lunchmoney.CategoryFormatFlattened})
	if err != nil {
		return nil, fmt.Errorf("get categories: %w", err)
	}
	byID := map[int64]*lunchmoney.Category{}
	for _, cat := range categories {
		if cat != nil {
			byID[cat.ID] = cat
		}
	}

	txs, err := fetchTransactions(ctx, c, start, now)
	if err != nil {
		return nil, err
	}

	p := &Plan{
		GeneratedAt: now.UTC(),
		PlanFor:     current.AddDate(0, 1, 0).Format(monthKey),
		Months:      []string{},
		Lately:      []string{},
		Changes:     []Change{},
		Income:      []IncomeLine{},
		Bills:       []BillLine{},
		Caps:        []CapLine{},
		Stopped:     []StoppedBill{},
		OneOffs:     []OneOff{},
		Options:     []Option{},
		ChosenBy:    "rules",
	}
	p.Totals.Target = opts.SavingsTarget

	charges, deposits, skipped := classify(txs, byID, opts)

	active := map[string]bool{}
	for _, ch := range charges {
		active[ch.month] = true
	}
	for _, d := range deposits {
		active[d.date.Format(monthKey)] = true
	}
	for m := start; m.Before(current); m = m.AddDate(0, 1, 0) {
		key := m.Format(monthKey)
		// Months before the history begins would drag every median to zero.
		if len(p.Months) == 0 && !active[key] {
			continue
		}
		p.Months = append(p.Months, key)
	}
	if len(p.Months) == 0 {
		p.Notes = append(p.Notes, "there is no complete month of transactions yet, so there is nothing to plan from")
		return p, nil
	}

	scale := latelyMonths(p, opts, now)
	if len(p.Lately) == 0 {
		p.Notes = append(p.Notes, "no month since ACNTNG_SINCE has enough data yet, so there is nothing to plan from")
		return p, nil
	}

	var loans []Loan
	if rep, err := BuildReport(ctx, c, now, Options{IncludeCredit: true}); err != nil {
		p.Notes = append(p.Notes, "loan balances could not be read, so a debt paid off in a lump is still budgeted")
	} else {
		loans = rep.Loans
	}

	p.Income = income(deposits, p.Lately, scale, now)
	b := bills(charges, now, p.Months, loans)
	p.Bills, p.Stopped, p.OneOffs = b.lines, b.stopped, b.oneOffs
	p.OneOffs = append(p.OneOffs, markOneOffs(charges, scale)...)
	sortOneOffs(p.OneOffs)
	if len(p.OneOffs) > maxOneOffs {
		p.OneOffs = p.OneOffs[:maxOneOffs]
	}
	p.Caps = caps(charges, p.Months, p.Lately, scale)

	t := &p.Totals
	for _, l := range p.Income {
		t.Income += l.Monthly
	}
	for i := range p.Bills {
		p.Bills[i].Plan = p.Bills[i].Monthly
		t.Bills += p.Bills[i].Monthly
	}
	for _, l := range p.Caps {
		t.Spending += l.Lately
	}
	t.Income, t.Bills, t.Spending = round2(t.Income), round2(t.Bills), round2(t.Spending)
	t.Unchanged = round2(t.Income - t.Bills - t.Spending)

	p.Options = options(p, charges, scale, b, opts)
	p.ChosenBy = "rules"
	p.Changes = changes(p, charges, scale, b, opts)
	t.Planned = p.planned()
	t.Savings = round2(t.Planned - t.Unchanged)
	t.Short = round2(max(t.Target-t.Planned, 0))

	p.notes(charges, scale, skipped)
	return p, nil
}

// classify splits transactions into spending charges and paychecks. Pending
// rows, split parents, grouped children and transfers are dropped.
func classify(txs []*lunchmoney.Transaction, byID map[int64]*lunchmoney.Category, opts PlanOptions) ([]charge, []deposit, int) {
	var charges []charge
	var deposits []deposit
	skipped := 0
	for _, tx := range txs {
		if tx == nil || tx.IsPending || tx.IsSplitParent || tx.GroupParentID != nil || len(tx.Date) < 10 {
			continue
		}
		date, err := time.Parse(dayKey, tx.Date[:10])
		if err != nil {
			continue
		}
		var cat *lunchmoney.Category
		if tx.CategoryID != nil {
			cat = byID[*tx.CategoryID]
		}
		// Transfers and card payments settle spending counted elsewhere.
		if cat != nil && (cat.ExcludeFromTotals || cat.ExcludeFromBudget) {
			continue
		}
		month := tx.Date[:7]

		if cat != nil && cat.IsIncome {
			amount := -tx.ToBase
			if amount > 0 && (opts.PaycheckMatch.MatchString(tx.Payee) || opts.PaycheckMatch.MatchString(tx.OriginalName)) {
				deposits = append(deposits, deposit{date: date, source: sourceName(tx.Payee), amount: amount})
			}
			continue
		}

		name := uncategorizedName
		if cat != nil {
			name = cat.Name
		} else if tx.ToBase < 0 {
			// An uncategorized credit is as likely a transfer as a refund.
			skipped++
			continue
		}

		payee := strings.TrimSpace(tx.Payee)
		if payee == "" {
			payee = strings.TrimSpace(tx.OriginalName)
		}
		ch := charge{
			date:     date,
			month:    month,
			payee:    payee,
			key:      payeeKey(payee),
			category: name,
			amount:   tx.ToBase,
			debt:     isDebt(name),
		}
		ch.fixed = ch.debt || opts.Fixed[strings.ToLower(strings.TrimSpace(name))]
		if !ch.debt {
			ch.fee = feeKind(payee+" "+tx.OriginalName, name)
		}
		if ch.fee != "" {
			ch.fixed = false
		}
		charges = append(charges, ch)
	}
	return charges, deposits, skipped
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

// latelyMonths picks the months that describe how things are now and returns
// the factor each is scaled by: one for a complete month, more for the month
// in progress. With Since set, nothing before it counts.
func latelyMonths(p *Plan, opts PlanOptions, now time.Time) map[string]float64 {
	scale := map[string]float64{}
	since := ""
	if !opts.Since.IsZero() {
		since = monthStart(opts.Since).Format(monthKey)
		for _, m := range p.Months {
			if m >= since {
				p.Lately = append(p.Lately, m)
			}
		}
	} else {
		p.Lately = append(p.Lately, p.Months[max(len(p.Months)-latelyFallbackMonths, 0):]...)
	}
	for _, m := range p.Lately {
		scale[m] = 1
	}
	if key := now.Format(monthKey); now.Day() >= partialMonthMinDay && key >= since {
		days := monthStart(now).AddDate(0, 1, -1).Day()
		p.Lately = append(p.Lately, key)
		scale[key] = float64(days) / float64(now.Day())
	}
	return scale
}

// income expects each employer still paying to pay what it paid in a typical
// recent month since it started. A source quiet for 45 days is not counted on.
func income(deposits []deposit, lately []string, scale map[string]float64, now time.Time) []IncomeLine {
	type source struct {
		months      map[string]float64
		first, last time.Time
	}
	bySource := map[string]*source{}
	for _, d := range deposits {
		s := bySource[d.source]
		if s == nil {
			s = &source{months: map[string]float64{}, first: d.date, last: d.date}
			bySource[d.source] = s
		}
		s.months[d.date.Format(monthKey)] += d.amount
		if d.date.Before(s.first) {
			s.first = d.date
		}
		if d.date.After(s.last) {
			s.last = d.date
		}
	}
	quiet := now.AddDate(0, 0, -stoppedAfterDays)
	out := []IncomeLine{}
	for name, s := range bySource {
		if s.last.Before(quiet) {
			continue
		}
		started := s.first.Format(monthKey)
		// Pay arrives on a schedule, so the month in progress is only read
		// when it is all that exists.
		var v, partial []float64
		for _, m := range lately {
			if m < started {
				continue
			}
			if scale[m] == 1 {
				v = append(v, s.months[m])
			} else {
				partial = append(partial, s.months[m])
			}
		}
		if len(v) == 0 {
			v = partial
		}
		monthly := round2(median(v))
		if monthly <= 0 {
			continue
		}
		out = append(out, IncomeLine{Name: name, Monthly: monthly})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Monthly > out[j].Monthly })
	return out
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

// payeeKey groups one payee's charges whatever trace numbers and recipient
// names the bank adds: its first two words without digits.
func payeeKey(payee string) string {
	words := strings.Fields(strings.ToLower(sourceName(payee)))
	return strings.Join(words[:min(len(words), 2)], " ")
}

// displayName shortens a bank descriptor full of trace numbers to its words.
func displayName(payee string) string {
	if !strings.ContainsAny(payee, "0123456789") {
		return payee
	}
	return sourceName(payee)
}

// monthly totals values by month, in the order of months, zero when absent.
func monthly(by map[string]float64, months []string, scale map[string]float64) []float64 {
	out := make([]float64, 0, len(months))
	for _, m := range months {
		f := 1.0
		if scale != nil {
			f = scale[m]
		}
		out = append(out, round2(by[m]*f))
	}
	return out
}

func sortOneOffs(o []OneOff) {
	sort.SliceStable(o, func(i, j int) bool { return o[i].Amount > o[j].Amount })
}

func (p *Plan) notes(charges []charge, scale map[string]float64, skipped int) {
	if len(p.Months) < 6 {
		p.Notes = append(p.Notes, fmt.Sprintf("only %d months of history exist, so usual spending is a rough guess", len(p.Months)))
	}
	if len(p.Income) == 0 {
		p.Notes = append(p.Notes, "no paychecks matched, so income is zero; set ACNTNG_PAYCHECK_MATCH to a pattern that matches the payroll payee")
	}
	var total, unc float64
	count := 0
	for _, ch := range charges {
		if _, ok := scale[ch.month]; !ok {
			continue
		}
		if ch.fixed {
			continue
		}
		total += ch.amount
		if ch.category == uncategorizedName {
			unc += ch.amount
			count++
		}
	}
	if total > 0 && unc/total >= 0.05 {
		p.Notes = append(p.Notes, fmt.Sprintf(
			"%d recent transactions worth $%s have no category; they count as spending but cannot be planned until they are categorized",
			count, money(unc)))
	}
	if skipped > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf(
			"%d uncategorized credits were left out, since an unlabelled deposit is as likely a transfer as a refund", skipped))
	}
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

// planned is what the month leaves with every bill and cap as planned.
func (p *Plan) planned() float64 {
	left := p.Totals.Income
	for _, l := range p.Bills {
		left -= l.Plan
	}
	for _, c := range p.Caps {
		left -= c.Cap
	}
	return round2(left)
}

// clone copies the plan deeply enough to apply changes to the copy.
func (p *Plan) clone() *Plan {
	out := *p
	out.Caps = append([]CapLine{}, p.Caps...)
	out.Bills = append([]BillLine{}, p.Bills...)
	out.Changes = append([]Change{}, p.Changes...)
	out.Notes = append([]string{}, p.Notes...)
	return &out
}
