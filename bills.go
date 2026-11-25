package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// billResult is what bills found: what is due, what stopped, and the lumps
// left out of both.
type billResult struct {
	lines   []BillLine
	stopped []StoppedBill
	oneOffs []OneOff
	// recurring holds flexible payees that charge every month, such as
	// subscriptions, keyed by payee.
	recurring map[string]BillLine
}

type payeeCharges struct {
	key     string
	name    string
	charges []charge
	spike   []bool
}

// groupByPayee gathers the kept charges by payee. A one-word payee joins a
// longer one that starts with it, since banks and rules name the same lender
// both ways.
func groupByPayee(charges []charge, keep func(charge) bool) []*payeeCharges {
	keys := map[string]bool{}
	for _, ch := range charges {
		if keep(ch) {
			keys[ch.key] = true
		}
	}
	longer := map[string]string{}
	for k := range keys {
		if first, _, ok := strings.Cut(k, " "); ok && keys[first] && (longer[first] == "" || k < longer[first]) {
			longer[first] = k
		}
	}

	by := map[string]*payeeCharges{}
	var order []string
	for _, ch := range charges {
		if !keep(ch) {
			continue
		}
		key := ch.key
		if l, ok := longer[key]; ok {
			key = l
		}
		g := by[key]
		if g == nil {
			g = &payeeCharges{key: key}
			by[key] = g
			order = append(order, key)
		}
		g.charges = append(g.charges, ch)
	}
	sort.Strings(order)
	out := make([]*payeeCharges, 0, len(order))
	for _, k := range order {
		g := by[k]
		sort.SliceStable(g.charges, func(i, j int) bool { return g.charges[i].date.Before(g.charges[j].date) })
		g.name = displayName(g.charges[len(g.charges)-1].payee)
		g.markSpikes()
		out = append(out, g)
	}
	return out
}

// markSpikes flags payoffs and deposits: charges far above the payee's other
// charges. A debt payment must also be large, so a raised monthly payment
// is not mistaken for a payoff.
func (g *payeeCharges) markSpikes() {
	g.spike = make([]bool, len(g.charges))
	for i, ch := range g.charges {
		if ch.amount < oneOffMin || (ch.debt && ch.amount < payoffMin) {
			continue
		}
		var others []float64
		for j, o := range g.charges {
			if j != i && o.amount > 0 {
				others = append(others, o.amount)
			}
		}
		g.spike[i] = len(others) > 0 && ch.amount > median(others)*spikeRatio
	}
}

func (g *payeeCharges) category() string { return g.charges[len(g.charges)-1].category }

// regularMonthly is the median of the last three months the payee charged in,
// leaving out spikes, over the last half year.
func (g *payeeCharges) regularMonthly(now time.Time) float64 {
	from := now.AddDate(0, -6, 0)
	by := map[string]float64{}
	var order []string
	for i, ch := range g.charges {
		if ch.date.Before(from) || g.spike[i] {
			continue
		}
		if _, ok := by[ch.month]; !ok {
			order = append(order, ch.month)
		}
		by[ch.month] += ch.amount
	}
	var v []float64
	for _, m := range order[max(len(order)-3, 0):] {
		v = append(v, by[m])
	}
	return round2(median(v))
}

// cadenceDays is the median gap between the payee's regular charges.
func (g *payeeCharges) cadenceDays() float64 {
	var gaps []float64
	var prev time.Time
	for i, ch := range g.charges {
		if g.spike[i] {
			continue
		}
		if !prev.IsZero() && ch.date.Sub(prev).Hours() >= 24*7 {
			gaps = append(gaps, ch.date.Sub(prev).Hours()/24)
		}
		prev = ch.date
	}
	return median(gaps)
}

func (g *payeeCharges) months(from time.Time) map[string]bool {
	out := map[string]bool{}
	for i, ch := range g.charges {
		if !ch.date.Before(from) && !g.spike[i] {
			out[ch.month] = true
		}
	}
	return out
}

// bills sorts fixed costs into what is due next month, what has stopped, and
// lumps such as payoffs and deposits. Occasional costs in a fixed category,
// such as a doctor's bill, are budgeted at their average month.
func bills(charges []charge, now time.Time, months []string, loans []Loan) billResult {
	res := billResult{recurring: map[string]BillLine{}}
	lookback := now.AddDate(0, 0, -billLookbackDays)
	quiet := now.AddDate(0, 0, -stoppedAfterDays)
	occasional := map[string]float64{}
	complete := map[string]bool{}
	for _, m := range months {
		complete[m] = true
	}

	for _, g := range groupByPayee(charges, func(ch charge) bool { return ch.fixed }) {
		last := g.charges[len(g.charges)-1]
		seen := g.months(time.Time{})
		recent := g.months(lookback)
		debt := last.debt

		for i, ch := range g.charges {
			if ch.date.Before(lookback) || !g.spike[i] {
				continue
			}
			reason := "larger than this payee's usual charge"
			if debt {
				reason = "extra payment toward the balance"
			}
			res.oneOffs = append(res.oneOffs, OneOff{
				Date: ch.date.Format(dayKey), Payee: displayName(ch.payee), Category: ch.category, Amount: round2(ch.amount), Reason: reason,
			})
		}

		monthlyNow := len(recent) >= recurringMinMonths && !last.date.Before(quiet)
		if !debt && !monthlyNow && len(seen) <= occasionalMaxMonths {
			for i, ch := range g.charges {
				if !g.spike[i] && complete[ch.month] {
					occasional[ch.category] += ch.amount
				}
			}
			continue
		}

		amount := g.regularMonthly(now)
		line := BillLine{Name: g.name, Category: g.category(), Monthly: amount, Debt: debt, FirstSeen: g.charges[0].month}
		stop := func(reason string) {
			if amount > 0 {
				res.stopped = append(res.stopped, StoppedBill{Name: g.name, Category: line.Category, Monthly: amount, Reason: reason})
			}
		}
		gap := g.cadenceDays()

		balance, matched := loanBalance(g.key, loans)
		switch {
		case debt && matched && balance <= 0:
			stop("paid off")
		case monthlyNow || (debt && !last.date.Before(quiet)):
			if amount > 0 {
				res.lines = append(res.lines, line)
			}
		case !debt && gap > stoppedAfterDays && now.Sub(last.date).Hours()/24 <= gap*1.5:
			// Quarterly or yearly and still running: set aside its average
			// month so the next charge is covered.
			total := 0.0
			for i, ch := range g.charges {
				if !g.spike[i] && complete[ch.month] {
					total += ch.amount
				}
			}
			line.Monthly = round2(total / float64(len(months)))
			line.Name = fmt.Sprintf("%s, about every %d months", g.name, int(gap/30+0.5))
			if line.Monthly >= 1 {
				res.lines = append(res.lines, line)
			}
		case last.date.Before(lookback):
		default:
			stop("nothing charged since " + last.date.Format("2 Jan"))
		}
	}

	for cat, total := range occasional {
		avg := round2(total / float64(len(months)))
		if avg < 1 {
			continue
		}
		res.lines = append(res.lines, BillLine{Name: "Occasional " + strings.ToLower(cat), Category: cat, Monthly: avg, Occasional: true})
	}

	for _, g := range groupByPayee(charges, func(ch charge) bool { return !ch.fixed && ch.fee == "" }) {
		last := g.charges[len(g.charges)-1]
		if last.date.Before(quiet) || len(g.months(lookback)) < recurringMinMonths {
			continue
		}
		if !steady(g, lookback) {
			continue
		}
		res.recurring[g.key] = BillLine{
			Name: g.name, Category: g.category(), Monthly: g.regularMonthly(now), FirstSeen: g.charges[0].month,
		}
	}

	sort.SliceStable(res.lines, func(i, j int) bool {
		if res.lines[i].Debt != res.lines[j].Debt {
			return !res.lines[i].Debt
		}
		return res.lines[i].Monthly > res.lines[j].Monthly
	})
	sort.SliceStable(res.stopped, func(i, j int) bool { return res.stopped[i].Monthly > res.stopped[j].Monthly })
	return res
}

// steady reports whether a payee charges about the same once a month, which
// is what a subscription or service plan looks like.
func steady(g *payeeCharges, from time.Time) bool {
	by := map[string][]float64{}
	for _, ch := range g.charges {
		if !ch.date.Before(from) {
			by[ch.month] = append(by[ch.month], ch.amount)
		}
	}
	lo, hi := 0.0, 0.0
	for _, v := range by {
		if len(v) != 1 {
			return false
		}
		if lo == 0 || v[0] < lo {
			lo = v[0]
		}
		hi = max(hi, v[0])
	}
	return lo > 0 && hi <= lo*1.25
}

// loanBalance finds the loans a payee pays, by the payee's first word, and
// totals what is still owed on them.
func loanBalance(key string, loans []Loan) (float64, bool) {
	word := strings.Fields(key)
	if len(word) == 0 {
		return 0, false
	}
	needle := normalize(word[0])
	if len(needle) < 4 {
		return 0, false
	}
	total, matched := 0.0, false
	for _, l := range loans {
		if strings.Contains(normalize(l.Name+" "+l.DisplayName+" "+l.InstitutionName), needle) {
			matched = true
			total += l.Balance
		}
	}
	return total, matched
}

// markOneOffs flags big recent purchases from payees that never charge that
// much again. They are listed, and left out of lately so a move or a flight
// is not planned for twice.
func markOneOffs(charges []charge, lately map[string]float64) []OneOff {
	byKey := map[string][]int{}
	for i, ch := range charges {
		if !ch.fixed && ch.fee == "" {
			byKey[ch.key] = append(byKey[ch.key], i)
		}
	}
	var out []OneOff
	for _, idx := range byKey {
		for _, i := range idx {
			ch := charges[i]
			if _, ok := lately[ch.month]; !ok || ch.amount < oneOffMin {
				continue
			}
			repeated := slices.ContainsFunc(idx, func(j int) bool {
				return j != i && charges[j].amount >= ch.amount*0.75 && charges[j].amount <= ch.amount*1.25
			})
			if repeated {
				continue
			}
			charges[i].oneOff = true
			out = append(out, OneOff{
				Date: ch.date.Format(dayKey), Payee: displayName(ch.payee), Category: ch.category, Amount: round2(ch.amount),
				Reason: "one-time purchase",
			})
		}
	}
	return out
}

func describeMonth(m string) string {
	t, err := time.Parse(monthKey, m)
	if err != nil {
		return m
	}
	return t.Format("January 2006")
}

func describeCount(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
