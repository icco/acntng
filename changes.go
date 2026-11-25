package main

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

const (
	// creepRatio and minCreep keep ordinary wobble from reading as a habit
	// that grew.
	creepRatio = 1.10
	minCreep   = 50.0
	minCut     = 50.0
	minFee     = 5.0
	maxNamed   = 2
)

// Change kinds.
const (
	KindFee     = "fee"
	KindOldBill = "old_bill"
	KindCreep   = "creep"
	KindCut     = "cut"
)

// Fee kinds, in the order they are reported.
const (
	feeInterest  = "interest"
	feeForeign   = "foreign"
	feeOverdraft = "overdraft"
	feeService   = "service"
	feeOther     = "other"
)

var (
	feeCategory = regexp.MustCompile(`(?i)\bfees?\b`)
	feePatterns = []struct {
		kind string
		re   *regexp.Regexp
	}{
		{feeInterest, regexp.MustCompile(`(?i)\binterest (charge|adj)|finance charge`)},
		{feeForeign, regexp.MustCompile(`(?i)foreign (currency|transaction)|international (purchase|transaction)|\bfx fee|cross.?border`)},
		{feeOverdraft, regexp.MustCompile(`(?i)overdraft|\bnsf\b|insufficient funds|return(ed)? payment|late (payment )?fee`)},
		{feeService, regexp.MustCompile(`(?i)(service|maintenance|account) (fee|charge)|wire .*(fee|charge)|\batm\b.*fee`)},
	}
	// housing matches the categories a home's own bills land in.
	housing = regexp.MustCompile(`(?i)rent|utilit|home|insurance|phone|internet|security`)
)

// feeKind names the kind of fee a charge is, or "" when it is not one. Text
// is the payee and original description; anything in a category named for
// fees is a fee.
func feeKind(text, category string) string {
	for _, p := range feePatterns {
		if p.re.MatchString(text) {
			return p.kind
		}
	}
	if feeCategory.MatchString(category) {
		return feeOther
	}
	return ""
}

// Change is one thing to do differently next month, with what it saves.
type Change struct {
	Kind     string  `json:"kind"`
	Title    string  `json:"title"`
	Detail   string  `json:"detail"`
	Category string  `json:"category,omitempty"`
	Saving   float64 `json:"saving"`
}

// caps builds a line per category of chosen spending: its usual month over
// the whole history and its typical month lately. Fees are one line.
func caps(charges []charge, months, lately []string, scale map[string]float64) []CapLine {
	by := map[string]map[string]float64{}
	recent := map[string]map[string]float64{}
	for _, ch := range charges {
		if ch.fixed {
			continue
		}
		cat := ch.category
		if ch.fee != "" {
			cat = feesName
		}
		if by[cat] == nil {
			by[cat] = map[string]float64{}
			recent[cat] = map[string]float64{}
		}
		by[cat][ch.month] += ch.amount
		if !ch.oneOff {
			recent[cat][ch.month] += ch.amount
		}
	}
	out := []CapLine{}
	for cat, m := range by {
		hist := monthly(m, months, nil)
		line := CapLine{
			Category: cat,
			Usual:    round2(max(median(hist), 0)),
			Low:      round2(max(quantile(hist, 0.25), 0)),
			Lately:   round2(max(median(monthly(recent[cat], lately, scale)), 0)),
		}
		line.Cap = line.Lately
		if line.Usual == 0 && line.Lately == 0 {
			continue
		}
		out = append(out, line)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lately != out[j].Lately {
			return out[i].Lately > out[j].Lately
		}
		return out[i].Category < out[j].Category
	})
	return out
}

// changes finds what to do differently, biggest saving first: fees, bills
// left over from before the change in how you live, spending that crept up,
// and then, only if those do not reach the target, cuts to the leanest
// quarter of months. Each change lowers the matching cap.
func changes(p *Plan, charges []charge, scale map[string]float64, b billResult, opts PlanOptions) []Change {
	capOf := map[string]*CapLine{}
	for i := range p.Caps {
		capOf[p.Caps[i].Category] = &p.Caps[i]
	}
	// lower takes up to by off a cap and returns what it actually took, so a
	// change never claims more than its cap held.
	lower := func(cat string, by float64) float64 {
		c := capOf[cat]
		if c == nil {
			return 0
		}
		took := min(by, c.Cap)
		c.Cap = round2(c.Cap - took)
		return round2(took)
	}

	var out []Change
	for _, ch := range feeChanges(charges, p.Lately, scale) {
		if ch.Saving = lower(feesName, ch.Saving); ch.Saving >= minFee {
			out = append(out, ch)
		}
	}

	if !opts.Since.IsZero() {
		since := monthStart(opts.Since).Format(monthKey)
		old := func(l BillLine) bool {
			return l.FirstSeen < since && housing.MatchString(l.Category) && l.Monthly >= minFee
		}
		oldBill := func(l BillLine, saving float64) Change {
			return Change{
				Kind:     KindOldBill,
				Category: l.Category,
				Title:    fmt.Sprintf("Cancel %s if it is for the old place", l.Name),
				Detail: fmt.Sprintf("Still charging about $%s a month, and it started before your move in %s.",
					money(l.Monthly), describeMonth(since)),
				Saving: saving,
			}
		}
		for i := range p.Bills {
			if l := &p.Bills[i]; !l.Debt && !l.Occasional && old(*l) {
				l.Plan = 0
				out = append(out, oldBill(*l, l.Monthly))
			}
		}
		keys := make([]string, 0, len(b.recurring))
		for k := range b.recurring {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if l := b.recurring[k]; old(l) {
				if took := lower(l.Category, l.Monthly); took >= minFee {
					out = append(out, oldBill(l, took))
				}
			}
		}
	}

	for i := range p.Caps {
		c := &p.Caps[i]
		if c.Category == feesName || c.Category == uncategorizedName {
			continue
		}
		over := c.Cap - c.Usual
		if c.Lately <= c.Usual*creepRatio || over < minCreep {
			continue
		}
		detail := fmt.Sprintf("Lately $%s a month, against a usual $%s across %d months.",
			money(c.Lately), money(c.Usual), len(p.Months))
		if named := topPayees(charges, c.Category, scale); named != "" {
			detail += " Most of it: " + named + "."
		}
		out = append(out, Change{
			Kind:     KindCreep,
			Category: c.Category,
			Title:    fmt.Sprintf("Bring %s back to $%s", c.Category, money(c.Usual)),
			Detail:   detail,
			Saving:   round2(over),
		})
		c.Cap = c.Usual
	}

	out = cuts(p, p.Totals.Target-p.planned(), out)

	for i := range out {
		out[i].Saving = round2(out[i].Saving)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Saving > out[j].Saving })
	return out
}

// cuts closes what is left of the gap by holding the categories with the most
// room to their leanest quarter of months, largest first.
func cuts(p *Plan, gap float64, out []Change) []Change {
	if gap <= 0 {
		return out
	}
	var order []*CapLine
	for i := range p.Caps {
		c := &p.Caps[i]
		if c.Category == feesName || c.Category == uncategorizedName || c.Cap-c.Low < minCut {
			continue
		}
		order = append(order, c)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].Cap-order[i].Low > order[j].Cap-order[j].Low })

	for _, c := range order {
		if gap <= 0 {
			break
		}
		saving := min(c.Cap-c.Low, gap)
		target := round2(c.Cap - saving)
		gap -= saving
		// A category already asked to come back to usual is asked once, for
		// the lower figure.
		if i := slices.IndexFunc(out, func(ch Change) bool { return ch.Kind == KindCreep && ch.Category == c.Category }); i >= 0 {
			out[i].Title = fmt.Sprintf("Hold %s to $%s", c.Category, money(target))
			out[i].Detail += fmt.Sprintf(" To reach the target, hold it to $%s; a quarter of your months came in at $%s or less.",
				money(target), money(c.Low))
			out[i].Saving += saving
			c.Cap = target
			continue
		}
		out = append(out, Change{
			Kind:     KindCut,
			Category: c.Category,
			Title:    fmt.Sprintf("Hold %s to $%s", c.Category, money(target)),
			Detail: fmt.Sprintf("Lately $%s a month. Holding it to $%s reaches the target; a quarter of your months came in at $%s or less.",
				money(c.Lately), money(target), money(c.Low)),
			Saving: saving,
		})
		c.Cap = target
	}
	return out
}

// feeChanges turns each kind of fee charged lately into a change.
func feeChanges(charges []charge, lately []string, scale map[string]float64) []Change {
	by := map[string]map[string]float64{}
	counts := map[string]int{}
	for _, ch := range charges {
		if ch.fee == "" {
			continue
		}
		if by[ch.fee] == nil {
			by[ch.fee] = map[string]float64{}
		}
		by[ch.fee][ch.month] += ch.amount
		if _, ok := scale[ch.month]; ok {
			counts[ch.fee]++
		}
	}
	var out []Change
	for _, kind := range []string{feeInterest, feeForeign, feeOverdraft, feeService, feeOther} {
		amount := round2(median(monthly(by[kind], lately, scale)))
		if amount < minFee {
			continue
		}
		n := counts[kind]
		ch := Change{Kind: KindFee, Category: feesName, Saving: amount}
		switch kind {
		case feeInterest:
			ch.Title = "Stop paying card interest"
			ch.Detail = fmt.Sprintf("%s cost $%s a month lately. Pay statement balances in full, starting with the card charging it.",
				describeCount(n, "interest charge", "interest charges"), money(amount))
		case feeForeign:
			ch.Title = "Use a card with no foreign transaction fees"
			ch.Detail = fmt.Sprintf("%s cost $%s a month lately. Pay abroad with a card that charges no foreign transaction fee.",
				describeCount(n, "foreign transaction fee", "foreign transaction fees"), money(amount))
		case feeOverdraft:
			ch.Title = "Stop overdraft and returned-payment fees"
			ch.Detail = fmt.Sprintf("%s cost $%s a month lately. Keep a buffer in checking, or move autopay dates after payday.",
				describeCount(n, "fee", "fees"), money(amount))
		case feeService:
			ch.Title = "Drop account service fees"
			ch.Detail = fmt.Sprintf("%s cost $%s a month lately. Meet the fee waiver or move to an account without one.",
				describeCount(n, "service fee", "service fees"), money(amount))
		default:
			ch.Title = "Avoid the other fees"
			ch.Detail = fmt.Sprintf("%s cost $%s a month lately.", describeCount(n, "other fee", "other fees"), money(amount))
		}
		out = append(out, ch)
	}
	return out
}

// topPayees names the payees that took most of a category lately.
func topPayees(charges []charge, category string, scale map[string]float64) string {
	type agg struct {
		name  string
		total float64
		n     int
	}
	by := map[string]*agg{}
	for _, ch := range charges {
		if ch.fixed || ch.oneOff || ch.fee != "" || ch.category != category {
			continue
		}
		if _, ok := scale[ch.month]; !ok {
			continue
		}
		a := by[ch.key]
		if a == nil {
			a = &agg{name: displayName(ch.payee)}
			by[ch.key] = a
		}
		a.total += ch.amount
		a.n++
	}
	list := make([]*agg, 0, len(by))
	for _, a := range by {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].total > list[j].total })
	var parts []string
	for _, a := range list {
		if len(parts) == maxNamed || a.total <= 0 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s $%s over %s", a.name, money(a.total), describeCount(a.n, "purchase", "purchases")))
	}
	return strings.Join(parts, ", ")
}
