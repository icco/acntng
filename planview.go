package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"

	"go.icco.me/gutil/logging"
	"go.icco.me/gutil/render"
	"go.uber.org/zap"
)

// plan returns next month's plan, from cache when fresh, with its changes
// chosen by the model when one is configured and by the rules otherwise.
func (s *Server) plan(ctx context.Context, now time.Time) (*Plan, error) {
	const key = "next"
	p, ok := s.planCache.get(key, now)
	if !ok {
		opts := s.PlanOptions
		opts.Now = now
		var err error
		if p, err = BuildPlan(ctx, s.Client, opts); err != nil {
			return nil, err
		}
		s.planCache.set(key, p, now)
	}
	return s.ranked(ctx, p, now), nil
}

// ranked has the model choose the plan's changes. When it cannot, the rules'
// choice stands and a note says why.
func (s *Server) ranked(ctx context.Context, p *Plan, now time.Time) *Plan {
	if s.Ranker == nil || len(p.Options) == 0 {
		return p
	}
	since := ""
	if !s.PlanOptions.Since.IsZero() {
		since = s.PlanOptions.Since.Format(monthKey)
	}
	out := p.clone()
	prompt, err := rankPrompt(p, since)
	var r *Ranking
	if err == nil {
		r, err = s.rank.get(ctx, s.Ranker, prompt, func(r *Ranking) error {
			if len(r.clean(p)) == 0 {
				return errors.New("the model picked no known option")
			}
			return nil
		}, now)
	}
	var picks []Pick
	if err == nil {
		picks = r.clean(p)
	}
	if err != nil {
		if s.Log != nil {
			s.Log.Warnw("could not rank changes; using the rules", zap.Error(err))
		}
		out.Notes = append(out.Notes, "the model could not rank the changes, so the rules chose them: "+err.Error())
		return out
	}
	out.apply(picks)
	out.ChosenBy, out.Summary = r.Model, r.Summary
	return out
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

	fmt.Fprintf(w, "\n=== %s ===\n\n  %s\n  %s\n", describeMonth(p.PlanFor), verdict(p), basis(p))
	if p.Summary != "" {
		fmt.Fprintf(w, "  %s\n", p.Summary)
	}

	fmt.Fprint(w, "\nCHANGES\n")
	for _, c := range p.Changes {
		fmt.Fprintf(w, "  %s\t%s/mo\n", c.Title, money(c.Saving))
		if c.Why != "" {
			fmt.Fprintf(w, "    %s\n", c.Why)
		}
		fmt.Fprintf(w, "    %s\n", c.Detail)
	}
	fmt.Fprintf(w, "  (chosen by %s)\n", p.ChosenBy)
	if len(p.Changes) == 0 {
		fmt.Fprint(w, "  none needed\n")
	}

	fmt.Fprintf(w, "\nBUDGET\n  Income\t%s\n", money(t.Income))
	for _, l := range p.Income {
		fmt.Fprintf(w, "    %s\t%s\n", l.Name, money(l.Monthly))
	}
	bills, spending := 0.0, 0.0
	for _, l := range p.Bills {
		bills += l.Plan
	}
	for _, c := range p.Caps {
		spending += c.Cap
	}
	fmt.Fprintf(w, "  Bills\t%s\n", money(-bills))
	for _, l := range p.Bills {
		fmt.Fprintf(w, "    %s\t%s\t(lately %s)\n", l.Name, money(l.Plan), money(l.Monthly))
	}
	fmt.Fprintf(w, "  Spending\t%s\n", money(-spending))
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
	switch {
	case len(p.Changes) == 0 && t.Short > 0:
		return fmt.Sprintf("%s leaves %s, $%s short of your $%s target, and no change found here closes it.",
			month, signedDollars(t.Planned), money(t.Short), money(t.Target))
	case len(p.Changes) == 0:
		return fmt.Sprintf("%s leaves %s with nothing to change.", month, signedDollars(t.Unchanged))
	case t.Short > 0:
		return fmt.Sprintf("Even with every change below, %s leaves %s, $%s short of your $%s target.",
			month, signedDollars(t.Planned), money(t.Short), money(t.Target))
	default:
		return fmt.Sprintf("Make the %s below and %s leaves %s.",
			describeCount(len(p.Changes), "change", "changes"), month, signedDollars(t.Planned))
	}
}

// basis is what the verdict rests on.
func basis(p *Plan) string {
	t := p.Totals
	out := fmt.Sprintf("With no changes it leaves %s.", signedDollars(t.Unchanged))
	if t.Short > 0 {
		out += billShare(p)
	}
	return out
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
