package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// essential matches bill categories a household cannot simply cancel.
var essential = regexp.MustCompile(`(?i)rent|mortgage|medical|health|child`)

const (
	// minOption is the smallest monthly saving worth offering as a lever.
	minOption    = 10.0
	maxMerchants = 20
)

// Option kinds: what an option changes when it is picked.
const (
	OptionFee          = "fee"
	OptionBill         = "bill"
	OptionCategory     = "category"
	OptionMerchant     = "merchant"
	OptionSubscription = "subscription"
)

// Option is one lever the plan could pull next month, with its saving worked
// out by code. A model ranks options; it never writes their figures.
type Option struct {
	ID       int    `json:"id"`
	Kind     string `json:"kind"`
	Subject  string `json:"subject"`
	Category string `json:"category,omitempty"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	// Target is the cap a category option sets.
	Target float64 `json:"target,omitempty"`
	Saving float64 `json:"saving"`
}

// options lists every lever worth pulling: fees to stop, bills and
// subscriptions to cancel, categories to bring down, and the merchants that
// drive them. It reads the plan before any change is applied.
func options(p *Plan, charges []charge, scale map[string]float64, b billResult, opts PlanOptions) []Option {
	var out []Option
	add := func(o Option) {
		o.Saving = round2(o.Saving)
		if o.Saving >= minOption {
			out = append(out, o)
		}
	}

	for _, ch := range feeChanges(charges, p.Lately, scale) {
		add(Option{Kind: OptionFee, Subject: ch.Title, Category: feesName, Title: ch.Title, Detail: ch.Detail, Saving: ch.Saving})
	}

	since := ""
	if !opts.Since.IsZero() {
		since = monthStart(opts.Since).Format(monthKey)
	}
	started := func(first string) string {
		if since != "" && first < since {
			return fmt.Sprintf(" It started before %s.", describeMonth(since))
		}
		return ""
	}
	for _, l := range p.Bills {
		// Rent and doctors are not cancelled to save money; with a move date
		// only bills older than it are offered.
		if l.Debt || l.Occasional || (since != "" && l.FirstSeen >= since) || (since == "" && essential.MatchString(l.Category)) {
			continue
		}
		add(Option{
			Kind: OptionBill, Subject: l.Name, Category: l.Category,
			Title:  fmt.Sprintf("Cancel %s", l.Name),
			Detail: fmt.Sprintf("A %s bill of about $%s a month.%s", strings.ToLower(l.Category), money(l.Monthly), started(l.FirstSeen)),
			Saving: l.Monthly,
		})
	}
	keys := make([]string, 0, len(b.recurring))
	for k := range b.recurring {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		l := b.recurring[k]
		add(Option{
			Kind: OptionSubscription, Subject: l.Name, Category: l.Category,
			Title:  fmt.Sprintf("Cancel %s", l.Name),
			Detail: fmt.Sprintf("Charges about $%s every month in %s.%s", money(l.Monthly), l.Category, started(l.FirstSeen)),
			Saving: l.Monthly,
		})
	}

	for _, c := range p.Caps {
		if c.Category == feesName || c.Category == uncategorizedName {
			continue
		}
		evidence := fmt.Sprintf("Lately $%s a month; usual $%s over %d months; leanest quarter $%s.",
			money(c.Lately), money(c.Usual), len(p.Months), money(c.Low))
		if c.Lately-c.Usual >= minOption {
			add(Option{
				Kind: OptionCategory, Subject: c.Category, Category: c.Category,
				Title: fmt.Sprintf("Bring %s back to $%s", c.Category, money(c.Usual)), Detail: evidence,
				Target: c.Usual, Saving: c.Lately - c.Usual,
			})
		}
		if c.Low < c.Usual && c.Lately-c.Low >= minOption {
			add(Option{
				Kind: OptionCategory, Subject: c.Category, Category: c.Category,
				Title: fmt.Sprintf("Hold %s to $%s", c.Category, money(c.Low)), Detail: evidence,
				Target: c.Low, Saving: c.Lately - c.Low,
			})
		}
	}

	// A merchant's saving is what it takes off its category's lately
	// figure, a median of scaled months, so it is worked out the same way.
	catMonths := map[string]map[string]float64{}
	for _, ch := range charges {
		if ch.fixed || ch.fee != "" || ch.oneOff {
			continue
		}
		if catMonths[ch.category] == nil {
			catMonths[ch.category] = map[string]float64{}
		}
		catMonths[ch.category][ch.month] += ch.amount
	}
	merchantSaving := func(m merchantStat, keep func(float64) float64) float64 {
		cat := catMonths[m.category]
		before := max(median(monthly(cat, p.Lately, scale)), 0)
		adjusted := map[string]float64{}
		for _, month := range p.Lately {
			adjusted[month] = cat[month] - m.byMonth[month] + keep(m.byMonth[month])
		}
		return before - max(median(monthly(adjusted, p.Lately, scale)), 0)
	}
	for _, m := range merchantStats(charges, p.Months, scale, b) {
		evidence := fmt.Sprintf("Lately %.0f purchases and $%s a month in %s; usual $%s a month.", m.count, money(m.lately), m.category, money(m.usual))
		if m.lately-m.usual >= minOption && m.usual > 0 {
			usual := m.usual
			add(Option{
				Kind: OptionMerchant, Subject: m.name, Category: m.category,
				Title:  fmt.Sprintf("Bring %s back to $%s a month", m.name, money(m.usual)),
				Detail: evidence,
				Saving: merchantSaving(m, func(v float64) float64 { return min(v, usual) }),
			})
		}
		add(Option{
			Kind: OptionMerchant, Subject: m.name, Category: m.category,
			Title:  fmt.Sprintf("Halve %s", m.name),
			Detail: evidence,
			Saving: merchantSaving(m, func(v float64) float64 { return v / 2 }),
		})
	}

	for i := range out {
		out[i].ID = i + 1
	}
	return out
}

type merchantStat struct {
	name, category       string
	lately, usual, count float64
	// byMonth is the merchant's spending in each lately month, unscaled.
	byMonth map[string]float64
}

// merchantStats finds the merchants that take the most chosen spending
// lately, as monthly averages. Subscriptions and one-offs are left out.
func merchantStats(charges []charge, months []string, scale map[string]float64, b billResult) []merchantStat {
	eff := 0.0
	for _, f := range scale {
		eff += 1 / f
	}
	inHistory := map[string]bool{}
	for _, m := range months {
		inHistory[m] = true
	}
	type agg struct {
		merchantStat
		recent, all float64
		n           int
	}
	by := map[string]*agg{}
	for _, ch := range charges {
		if ch.fixed || ch.fee != "" || ch.oneOff || ch.category == uncategorizedName {
			continue
		}
		if _, sub := b.recurring[ch.key]; sub {
			continue
		}
		a := by[ch.key+"\x00"+ch.category]
		if a == nil {
			a = &agg{merchantStat: merchantStat{name: displayName(ch.payee), category: ch.category, byMonth: map[string]float64{}}}
			by[ch.key+"\x00"+ch.category] = a
		}
		if _, ok := scale[ch.month]; ok {
			a.recent += ch.amount
			a.byMonth[ch.month] += ch.amount
			a.n++
		}
		if inHistory[ch.month] {
			a.all += ch.amount
		}
	}
	var out []merchantStat
	for _, a := range by {
		if eff == 0 || a.recent <= 0 {
			continue
		}
		a.lately = round2(a.recent / eff)
		a.count = float64(a.n) / eff
		if len(months) > 0 {
			a.usual = round2(a.all / float64(len(months)))
		}
		out = append(out, a.merchantStat)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].lately != out[j].lately {
			return out[i].lately > out[j].lately
		}
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].category < out[j].category
	})
	if len(out) > maxMerchants {
		out = out[:maxMerchants]
	}
	return out
}

// apply resets the plan to no changes and makes the picked options, in
// order. Each change is credited only what it actually removes, and a
// subject is changed once.
func (p *Plan) apply(picks []Pick) {
	byID := map[int]Option{}
	for _, o := range p.Options {
		byID[o.ID] = o
	}
	for i := range p.Caps {
		p.Caps[i].Cap = p.Caps[i].Lately
	}
	for i := range p.Bills {
		p.Bills[i].Plan = p.Bills[i].Monthly
	}
	capOf := map[string]*CapLine{}
	for i := range p.Caps {
		capOf[p.Caps[i].Category] = &p.Caps[i]
	}
	lower := func(cat string, by float64) float64 {
		c := capOf[cat]
		if c == nil {
			return 0
		}
		took := min(by, c.Cap)
		c.Cap = round2(c.Cap - took)
		return round2(took)
	}

	p.Changes = []Change{}
	done := map[string]bool{}
	// capped holds categories already set to a target. A merchant or
	// subscription picked after that would take its saving from money the
	// target already removed, so it is skipped. The other order is safe: the
	// category then gives only what is left above its target.
	capped := map[string]bool{}
	for _, pick := range picks {
		o, ok := byID[pick.ID]
		if !ok || done[o.Kind+o.Subject+"\x00"+o.Category] {
			continue
		}
		if (o.Kind == OptionMerchant || o.Kind == OptionSubscription) && capped[o.Category] {
			continue
		}
		took := 0.0
		switch o.Kind {
		case OptionFee:
			took = lower(feesName, o.Saving)
		case OptionBill:
			for i := range p.Bills {
				if b := &p.Bills[i]; b.Name == o.Subject && b.Plan > 0 {
					took, b.Plan = b.Plan, 0
					break
				}
			}
		case OptionCategory:
			if c := capOf[o.Category]; c != nil && c.Cap > o.Target {
				took = round2(c.Cap - o.Target)
				c.Cap = o.Target
			}
			capped[o.Category] = true
		case OptionMerchant, OptionSubscription:
			took = lower(o.Category, o.Saving)
		}
		if took < 1 {
			continue
		}
		done[o.Kind+o.Subject+"\x00"+o.Category] = true
		p.Changes = append(p.Changes, Change{
			Kind: o.Kind, Title: o.Title, Detail: o.Detail, Why: pick.Why, Category: o.Category, Saving: took,
		})
	}
	t := &p.Totals
	t.Planned = p.planned()
	t.Savings = round2(t.Planned - t.Unchanged)
	t.Short = round2(max(t.Target-t.Planned, 0))
}
