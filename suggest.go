package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	// overNormalRatio and minOverNormal keep ordinary month-to-month wobble
	// from being reported as overspending.
	overNormalRatio = 1.10
	minOverNormal   = 25.0
	minTrim         = 20.0
	maxTrims        = 3
	minTrimHistory  = 3
	habitMinCount   = 4
	habitMinTotal   = 60.0
	maxHabits       = 3
	// uncategorizedShare is the fraction of spend that makes labelling
	// transactions worth a suggestion of its own.
	uncategorizedShare = 0.05
)

// feeCategory matches whole words, so Coffee is not a fee.
var feeCategory = regexp.MustCompile(`(?i)\bfees?\b`)

// Suggestion kinds.
const (
	KindShortfall     = "shortfall"
	KindOverNormal    = "over_normal"
	KindTrim          = "trim"
	KindFees          = "fees"
	KindHabit         = "habit"
	KindSubscriptions = "subscriptions"
	KindUncategorized = "uncategorized"
)

// Suggestion is one thing to do differently next month.
type Suggestion struct {
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Category string `json:"category,omitempty"`
	// MonthlySavings is measured against the review month, and nil when the
	// saving depends on a choice the numbers cannot make.
	MonthlySavings *float64 `json:"monthly_savings"`
}

// suggest sets each flexible category's target and derives the rule-based
// suggestions. It costs nothing to run, so it is the floor the page always
// has; the model only rewords and ranks on top of it.
func suggest(p *Plan) {
	var perCategory, trims []Suggestion

	// Without history there is no normal: comparing against zero would call
	// every bill overspending. The month itself is the only honest target.
	if len(p.BaselineMonths) == 0 {
		for i := range p.Categories {
			p.Categories[i].Target = p.Categories[i].Review
		}
		p.Suggestions = append(p.Suggestions, habits(p.Merchants)...)
		if s, ok := uncategorized(p.Categories); ok {
			p.Suggestions = append(p.Suggestions, s)
		}
		return
	}

	for i := range p.Categories {
		line := &p.Categories[i]
		if line.Fixed || line.Name == uncategorizedName {
			continue
		}

		switch {
		case feeCategory.MatchString(line.Name):
			if line.Review <= 0 {
				continue
			}
			line.Target = 0
			perCategory = append(perCategory, Suggestion{
				Kind:           KindFees,
				Category:       line.Name,
				Title:          fmt.Sprintf("Avoid %s in fees", dollars(line.Review)),
				Detail:         "Fees buy nothing. Check which account or card charged them and whether a balance minimum, autopay, or a different account removes them.",
				MonthlySavings: new(line.Review),
			})

		case line.Review > line.Normal*overNormalRatio && line.Review-line.Normal >= minOverNormal:
			perCategory = append(perCategory, Suggestion{
				Kind:     KindOverNormal,
				Category: line.Name,
				Title:    fmt.Sprintf("Bring %s back to %s", line.Name, dollars(line.Normal)),
				Detail: fmt.Sprintf("%s ran %s against a normal %s.",
					line.Name, dollars(line.Review), dollars(line.Normal)),
				MonthlySavings: new(round2(line.Review - line.Normal)),
			})

		case len(line.History) >= minTrimHistory && line.Review-line.Low >= minTrim:
			trims = append(trims, Suggestion{
				Kind:     KindTrim,
				Category: line.Name,
				Title:    fmt.Sprintf("Hold %s to %s", line.Name, dollars(line.Low)),
				Detail: fmt.Sprintf("You spent %s; a quarter of recent months came in at %s or less.",
					dollars(line.Review), dollars(line.Low)),
				MonthlySavings: new(round2(line.Review - line.Low)),
			})
		}
	}

	// Asking every category to hit its best quarter at once is not a plan
	// anyone keeps, so only the largest few trims are proposed.
	bySavings(trims)
	if len(trims) > maxTrims {
		trims = trims[:maxTrims]
	}
	for _, s := range trims {
		for i := range p.Categories {
			if p.Categories[i].Name == s.Category {
				p.Categories[i].Target = p.Categories[i].Low
			}
		}
	}

	perCategory = append(perCategory, trims...)
	bySavings(perCategory)
	for _, s := range perCategory {
		p.Totals.PotentialSavings += *s.MonthlySavings
	}
	p.Totals.PotentialSavings = round2(p.Totals.PotentialSavings)

	p.Suggestions = append(p.Suggestions, perCategory...)
	p.Suggestions = append(p.Suggestions, habits(p.Merchants)...)
	if s, ok := subscriptions(p.Recurring); ok {
		p.Suggestions = append(p.Suggestions, s)
	}
	if s, ok := uncategorized(p.Categories); ok {
		p.Suggestions = append(p.Suggestions, s)
	}
}

// headline puts a shortfall first. It runs after the totals exist.
func headline(p *Plan) {
	t := p.Totals
	if t.PaycheckNormal <= 0 || t.MarginNormal >= 0 {
		return
	}
	gap := -t.MarginNormal
	// Only targets below normal close this gap; returning an overspent
	// category to normal just gets back to it.
	recovered := min(gap, max(t.SpendNormal-t.TargetSpend, 0))
	s := Suggestion{
		Kind:  KindShortfall,
		Title: fmt.Sprintf("A normal month spends %s more than the paychecks bring in", dollars(gap)),
		Detail: fmt.Sprintf("Paychecks are normally %s and spending %s. The targets below close %s of that gap.",
			dollars(t.PaycheckNormal), dollars(t.SpendNormal), dollars(recovered)),
	}
	p.Suggestions = append([]Suggestion{s}, p.Suggestions...)
}

// habits turns frequent small purchases into a concrete lever. Their savings
// sit inside a category's, so they are not added to the total again.
func habits(merchants []MerchantLine) []Suggestion {
	var out []Suggestion
	for _, m := range merchants {
		if m.Count < habitMinCount || m.Total < habitMinTotal {
			continue
		}
		out = append(out, Suggestion{
			Kind:     KindHabit,
			Category: m.Category,
			Title:    fmt.Sprintf("Skip one in three at %s", m.Name),
			Detail: fmt.Sprintf("%d purchases came to %s, about %s each.",
				m.Count, dollars(m.Total), dollars(m.Total/float64(m.Count))),
			MonthlySavings: new(round2(m.Total / 3)),
		})
		if len(out) == maxHabits {
			break
		}
	}
	return out
}

func subscriptions(recurring []RecurringCharge) (Suggestion, bool) {
	if len(recurring) == 0 {
		return Suggestion{}, false
	}
	total := 0.0
	var top []string
	for i, r := range recurring {
		total += r.Monthly
		if i < 5 {
			top = append(top, fmt.Sprintf("%s %s", r.Name, dollars(r.Monthly)))
		}
	}
	return Suggestion{
		Kind:  KindSubscriptions,
		Title: fmt.Sprintf("Review %d recurring charges costing %s a month", len(recurring), dollars(total)),
		Detail: "Largest: " + strings.Join(top, ", ") +
			". Cancel what went unused last month; each one saves its full price every month after.",
	}, true
}

func uncategorized(lines []CategoryLine) (Suggestion, bool) {
	spend := 0.0
	var unc *CategoryLine
	for i := range lines {
		spend += lines[i].Review
		if lines[i].Name == uncategorizedName {
			unc = &lines[i]
		}
	}
	if unc == nil || spend <= 0 || unc.Review/spend < uncategorizedShare {
		return Suggestion{}, false
	}
	return Suggestion{
		Kind:  KindUncategorized,
		Title: fmt.Sprintf("Categorize %d transactions worth %s", unc.Transactions, dollars(unc.Review)),
		Detail: fmt.Sprintf("That is %.0f%% of the month's spending with no category, so it is missing from every comparison here.",
			unc.Review/spend*100),
	}, true
}

func bySavings(s []Suggestion) {
	sort.SliceStable(s, func(i, j int) bool { return *s[i].MonthlySavings > *s[j].MonthlySavings })
}

func dollars(f float64) string { return "$" + money(f) }
