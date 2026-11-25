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

// planView is the plan page's view model.
type planView struct {
	*Plan
	Flexible []CategoryLine
	Fixed    []CategoryLine
	// AIEnabled says whether a model is configured at all, so the page can
	// tell "no advice yet" from "advice is off".
	AIEnabled bool
	AdviceErr string
}

// plan returns the rule-based plan for the month containing review, from
// cache when fresh. Advice is attached per request, never cached here.
func (s *Server) plan(ctx context.Context, review, now time.Time) (*Plan, error) {
	key := monthStart(review).Format(monthKey)
	if p, ok := s.planCache.get(key, now); ok {
		return p, nil
	}

	opts := s.PlanOptions
	opts.Review, opts.Now = review, now
	p, err := BuildPlan(ctx, s.Client, opts)
	if err != nil {
		return nil, err
	}

	s.planCache.set(key, p, now)
	return p, nil
}

// reviewMonthFromRequest reads month=YYYY-MM, defaulting to the last complete
// month: a month still in progress always looks frugal.
func reviewMonthFromRequest(r *http.Request, now time.Time) (time.Time, error) {
	if r.URL.Query().Get("month") == "" {
		return lastCompleteMonth(now), nil
	}
	return monthFromRequest(r, now)
}

// advisedPlan builds the plan and attaches advice. Only generate spends a
// model call; otherwise the stored advice is read. An advice failure is
// returned beside the plan rather than instead of it.
func (s *Server) advisedPlan(ctx context.Context, review, now time.Time, generate bool) (*Plan, error, error) {
	p, err := s.plan(ctx, review, now)
	if err != nil {
		return nil, err, nil
	}

	out := *p
	var adviceErr error
	if generate {
		out.Advice, adviceErr = s.Coach.Generate(ctx, p, now)
	}
	if out.Advice == nil {
		stored, err := s.Coach.Stored(ctx, p.Month)
		out.Advice, adviceErr = stored, errors.Join(adviceErr, err)
	}
	return &out, nil, adviceErr
}

// Advice outcomes carried across the redirect after a refresh.
const (
	adviceBudget = "budget"
	adviceEmpty  = "empty"
	adviceFailed = "failed"
)

// adviceCode reduces an advice error to something safe to put in a URL.
func adviceCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrAdviceBudget):
		return adviceBudget
	case errors.Is(err, ErrNothingToAdvise):
		return adviceEmpty
	default:
		return adviceFailed
	}
}

func adviceMessage(code string) string {
	switch code {
	case "":
		return ""
	case adviceBudget:
		return "This month's AI calls are used up, so no new advice was generated."
	case adviceEmpty:
		return "There is nothing to ask the model about for this month."
	default:
		return "The AI advice could not be generated."
	}
}

// handlePlanPage renders the plan as HTML.
func (s *Server) handlePlanPage(w http.ResponseWriter, r *http.Request) {
	log := logging.FromContext(r.Context())
	now := s.clock()

	review, err := reviewMonthFromRequest(r, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	p, err, adviceErr := s.advisedPlan(r.Context(), review, now, false)
	if err != nil {
		log.Errorw("could not build plan", zap.Error(err))
		http.Error(w, "could not read from Lunch Money", http.StatusBadGateway)
		return
	}
	// A failed refresh arrives as a code on the redirect back here.
	code := r.URL.Query().Get("advice")
	if adviceErr != nil {
		log.Warnw("could not read stored advice", zap.Error(adviceErr))
		code = adviceFailed
	}

	v := &planView{Plan: p, AIEnabled: s.Coach != nil, AdviceErr: adviceMessage(code)}
	for _, line := range p.Categories {
		if line.Fixed {
			v.Fixed = append(v.Fixed, line)
		} else {
			v.Flexible = append(v.Flexible, line)
		}
	}

	body, err := renderTemplate("plan.html", v)
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
	now := s.clock()

	review, err := reviewMonthFromRequest(r, now)
	if err != nil {
		render.JSON(log, w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	p, err, adviceErr := s.advisedPlan(r.Context(), review, now, false)
	if err != nil {
		log.Errorw("could not build plan", zap.Error(err))
		render.JSON(log, w, http.StatusBadGateway,
			map[string]string{"error": "could not read from lunch money"})
		return
	}
	if adviceErr != nil {
		log.Warnw("could not read stored advice", zap.Error(adviceErr))
		p.Notes = append(append([]string{}, p.Notes...), adviceMessage(adviceCode(adviceErr)))
	}

	render.JSON(log, w, http.StatusOK, p)
}

// handleAdviceRefresh spends one model call on new advice, then returns to the
// page. It is the only route that calls the model. The redirect is relative because the service is mounted on a sub-path
// it cannot see.
func (s *Server) handleAdviceRefresh(w http.ResponseWriter, r *http.Request) {
	log := logging.FromContext(r.Context())
	now := s.clock()

	review, err := reviewMonthFromRequest(r, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	_, err, adviceErr := s.advisedPlan(r.Context(), review, now, true)
	if err != nil {
		log.Errorw("could not build plan", zap.Error(err))
		http.Error(w, "could not read from Lunch Money", http.StatusBadGateway)
		return
	}
	location := "./?month=" + monthStart(review).Format(monthKey)
	if adviceErr != nil {
		log.Warnw("advice refresh failed", zap.Error(adviceErr))
		location += "&advice=" + adviceCode(adviceErr)
	}

	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusSeeOther)
}

// printPlan writes the plan as terminal text.
func printPlan(out io.Writer, p *Plan) error {
	t := p.Totals
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)

	fmt.Fprintf(w, "\n=== acntng plan for %s (reviewing %s) ===\n\n", p.PlanFor, p.Month)
	fmt.Fprintf(w, "  \tNormal\t%s\n", p.Month)
	fmt.Fprintf(w, "  Paychecks\t%s\t%s\n", money(t.PaycheckNormal), money(t.PaycheckReview))
	fmt.Fprintf(w, "  Fixed spend\t%s\t%s\n", money(t.FixedNormal), money(t.FixedReview))
	fmt.Fprintf(w, "  Flexible spend\t%s\t%s\n", money(t.FlexibleNormal), money(t.FlexibleReview))
	fmt.Fprintf(w, "  Left over\t%s\t%s\n", money(t.MarginNormal), money(t.MarginReview))
	fmt.Fprintf(w, "\n  Target spend %s, leaving %s; suggestions below save up to %s\n",
		money(t.TargetSpend), money(t.TargetMargin), money(t.PotentialSavings))

	if a := p.Advice; a != nil {
		fmt.Fprintf(w, "\nADVICE (%s, %d in / %d out tokens)\n  %s\n", a.Model, a.TokensIn, a.TokensOut, a.Summary)
		for _, s := range a.Suggestions {
			saving := ""
			if s.MonthlySavings != nil {
				saving = fmt.Sprintf(" (%s/mo)", money(*s.MonthlySavings))
			}
			fmt.Fprintf(w, "  - %s%s: %s %s\n", s.Title, saving, s.Action, s.Why)
		}
	}

	fmt.Fprint(w, "\nSUGGESTIONS\n")
	for _, s := range p.Suggestions {
		saving := ""
		if s.MonthlySavings != nil {
			saving = fmt.Sprintf(" (%s/mo)", money(*s.MonthlySavings))
		}
		fmt.Fprintf(w, "  - %s%s\n    %s\n", s.Title, saving, s.Detail)
	}

	fmt.Fprint(w, "\nCATEGORIES\n  Category\tNormal\tSpent\tvs normal\tTarget\t\n")
	for _, c := range p.Categories {
		kind := ""
		if c.Fixed {
			kind = "fixed"
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			c.Name, money(c.Normal), money(c.Review), money(c.Delta), money(c.Target), kind)
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
