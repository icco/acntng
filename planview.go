package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"

	"go.icco.me/gutil/logging"
	"go.icco.me/gutil/render"
	"go.uber.org/zap"
)

// plan returns next month's plan, from cache when fresh.
func (s *Server) plan(ctx context.Context, now time.Time) (*Plan, error) {
	const key = "next"
	if p, ok := s.planCache.get(key, now); ok {
		return p, nil
	}

	opts := s.PlanOptions
	opts.Now = now
	p, err := BuildPlan(ctx, s.Client, opts)
	if err != nil {
		return nil, err
	}

	s.planCache.set(key, p, now)
	return p, nil
}

// handlePlanPage renders the plan as HTML.
func (s *Server) handlePlanPage(w http.ResponseWriter, r *http.Request) {
	log := logging.FromContext(r.Context())

	p, err := s.plan(r.Context(), s.clock())
	if err != nil {
		log.Errorw("could not build plan", zap.Error(err))
		http.Error(w, "could not read from Lunch Money", http.StatusBadGateway)
		return
	}

	body, err := renderTemplate("plan.html", p)
	if err != nil {
		log.Errorw("could not render plan", zap.Error(err))
		http.Error(w, "could not render the plan", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(body); err != nil {
		log.Warnw("could not write plan response", zap.Error(err))
	}
}

// handlePlan returns the plan as JSON.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	log := logging.FromContext(r.Context())

	p, err := s.plan(r.Context(), s.clock())
	if err != nil {
		log.Errorw("could not build plan", zap.Error(err))
		render.JSON(log, w, http.StatusBadGateway,
			map[string]string{"error": "could not read from lunch money"})
		return
	}

	render.JSON(log, w, http.StatusOK, p)
}

// printPlan writes the plan as terminal text.
func printPlan(out io.Writer, p *Plan) error {
	t := p.Totals
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)

	fmt.Fprintf(w, "\n=== %s ===\n\n  %s\n", describeMonth(p.PlanFor), verdict(p))

	fmt.Fprint(w, "\nCHANGES\n")
	for _, c := range p.Changes {
		fmt.Fprintf(w, "  %s\t%s/mo\n    %s\n", c.Title, money(c.Saving), c.Detail)
	}
	if len(p.Changes) == 0 {
		fmt.Fprint(w, "  none needed\n")
	}

	fmt.Fprintf(w, "\nBUDGET\n  Income\t%s\n", money(t.Income))
	for _, l := range p.Income {
		fmt.Fprintf(w, "    %s\t%s\n", l.Name, money(l.Monthly))
	}
	fmt.Fprintf(w, "  Bills\t%s\n", money(-t.Bills))
	for _, l := range p.Bills {
		fmt.Fprintf(w, "    %s\t%s\n", l.Name, money(l.Monthly))
	}
	fmt.Fprintf(w, "  Spending\t%s\n", money(-(t.Spending - t.Savings)))
	for _, c := range p.Caps {
		fmt.Fprintf(w, "    %s\t%s\t(lately %s, usual %s)\n", c.Category, money(c.Cap), money(c.Lately), money(c.Usual))
	}
	fmt.Fprintf(w, "  Left over\t%s\n", money(t.Planned))

	if len(p.Stopped) > 0 {
		fmt.Fprint(w, "\nNO LONGER DUE\n")
		for _, s := range p.Stopped {
			fmt.Fprintf(w, "  %s\t%s\t%s\n", s.Name, money(s.Monthly), s.Reason)
		}
	}
	if len(p.OneOffs) > 0 {
		fmt.Fprint(w, "\nONE-OFFS, NOT PLANNED FOR AGAIN\n")
		for _, o := range p.OneOffs {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", o.Date, o.Payee, money(o.Amount), o.Reason)
		}
	}
	if len(p.Notes) > 0 {
		fmt.Fprint(w, "\nNOTES\n")
		for _, n := range p.Notes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
	fmt.Fprintln(w)

	if err := w.Flush(); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

// verdict is the plan in one sentence.
func verdict(p *Plan) string {
	t := p.Totals
	month := describeMonth(p.PlanFor)
	unchanged := fmt.Sprintf("If nothing changes, %s leaves %s.", month, signedDollars(t.Unchanged))
	switch {
	case len(p.Changes) == 0:
		return unchanged
	case t.Short > 0:
		return fmt.Sprintf("%s With every change below it leaves %s, still $%s short of $%s.%s",
			unchanged, signedDollars(t.Planned), money(t.Short), money(t.Target), billShare(p))
	default:
		return fmt.Sprintf("%s Make the %s below and it leaves %s.",
			unchanged, describeCount(len(p.Changes), "change", "changes"), signedDollars(t.Planned))
	}
}

// billShare says how much of the pay bills take, when that is what keeps the
// plan short.
func billShare(p *Plan) string {
	t := p.Totals
	if t.Income <= 0 {
		return ""
	}
	debt := 0.0
	for _, l := range p.Bills {
		if l.Debt {
			debt += l.Monthly
		}
	}
	return fmt.Sprintf(" Bills take $%s of the $%s pay, $%s of it debt payments, so the rest of the gap is in the bills.",
		money(t.Bills), money(t.Income), money(debt))
}

func signedDollars(f float64) string {
	if f < 0 {
		return "−$" + money(-f)
	}
	return "$" + money(f)
}
