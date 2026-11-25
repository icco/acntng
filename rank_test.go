package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeRanker picks options by subject, so tests do not depend on ids.
type fakeRanker struct {
	subjects []string
	err      error
	calls    int
	prompts  []string
	plan     *Plan
}

func (f *fakeRanker) Rank(_ context.Context, prompt string) (*Ranking, error) {
	f.calls++
	f.prompts = append(f.prompts, prompt)
	if f.err != nil {
		return nil, f.err
	}
	r := &Ranking{Summary: "Eat out less.", Model: "fake-model"}
	for _, s := range f.subjects {
		id := 999
		for _, o := range f.plan.Options {
			if o.Subject == s || o.Title == s {
				id = o.ID
				break
			}
		}
		r.Picks = append(r.Picks, Pick{ID: id, Why: "Because " + s + "."})
	}
	return r, nil
}

func rankedServer(t *testing.T, r *fakeRanker, maxCalls int) (*Server, *Plan) {
	t.Helper()
	base := buildFixturePlan(t, planFixture(), planOpts(1000))
	r.plan = base
	return &Server{
		Log:         zap.NewNop().Sugar(),
		Client:      planFixture(),
		PlanOptions: planOpts(1000),
		Now:         fixedNow(planNow),
		Ranker:      r,
		rank:        rankCache{maxCalls: maxCalls},
	}, base
}

func TestOptionsCoverEveryLever(t *testing.T) {
	p := buildFixturePlan(t, planFixture(), planOpts(1000))
	kinds := map[string]bool{}
	for _, o := range p.Options {
		kinds[o.Kind] = true
		if o.Subject == "Harbor Lettings" {
			t.Errorf("rent from after the move is not a lever: %+v", o)
		}
		if o.Saving < minOption {
			t.Errorf("option below the floor: %+v", o)
		}
	}
	for _, k := range []string{OptionFee, OptionBill, OptionCategory, OptionMerchant, OptionSubscription} {
		if !kinds[k] {
			t.Errorf("no %s option in %+v", k, p.Options)
		}
	}
}

func TestRankedPlanFollowsTheModel(t *testing.T) {
	r := &fakeRanker{subjects: []string{"Noodle Cart", "Old Town Water", "Restaurants", "Nothing Real"}}
	s, base := rankedServer(t, r, 4)

	p, err := s.plan(context.Background(), planNow)
	if err != nil {
		t.Fatal(err)
	}
	if p.ChosenBy != "fake-model" || p.Summary != "Eat out less." {
		t.Errorf("chosen by %q, summary %q", p.ChosenBy, p.Summary)
	}
	// Noodle Cart's own option and its category's cover the same money; the
	// category gives only what the merchant pick left.
	if len(p.Changes) < 2 || !strings.Contains(p.Changes[0].Title, "Noodle Cart") || p.Changes[1].Title != "Cancel Old Town Water" {
		t.Fatalf("changes = %+v", p.Changes)
	}
	if p.Changes[0].Why != "Because Noodle Cart." || p.Changes[1].Saving != 80 {
		t.Errorf("first two = %+v", p.Changes[:2])
	}
	left := p.Totals.Income
	for _, b := range p.Bills {
		left -= b.Plan
	}
	for _, c := range p.Caps {
		left -= c.Cap
	}
	saved := 0.0
	for _, c := range p.Changes {
		saved += c.Saving
	}
	if round2(left) != p.Totals.Planned || round2(saved) != p.Totals.Savings {
		t.Errorf("rows leave %v, planned %v; changes save %v, savings %v", left, p.Totals.Planned, saved, p.Totals.Savings)
	}
	if base.ChosenBy != "rules" || base.Changes[0].Why != "" {
		t.Errorf("ranking changed the cached plan: %+v", base.Changes)
	}
}

func TestRankingIsCachedAndCapped(t *testing.T) {
	r := &fakeRanker{subjects: []string{"Old Town Water"}}
	s, _ := rankedServer(t, r, 1)
	for range 3 {
		if _, err := s.plan(context.Background(), planNow); err != nil {
			t.Fatal(err)
		}
	}
	if r.calls != 1 {
		t.Errorf("model called %d times for the same facts on one day, want 1", r.calls)
	}

	// New facts on the same day are over the cap, so the rules answer.
	s.planCache = cache[*Plan]{}
	s.PlanOptions.SavingsTarget = 2000
	p, err := s.plan(context.Background(), planNow)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 || p.ChosenBy != "rules" || !strings.Contains(strings.Join(p.Notes, " "), "used up") {
		t.Errorf("calls %d, chosen by %s, notes %v", r.calls, p.ChosenBy, p.Notes)
	}
}

func TestRankingFailureFallsBackAndRetriesLater(t *testing.T) {
	r := &fakeRanker{err: errors.New("proxy down")}
	s, base := rankedServer(t, r, 5)

	p, err := s.plan(context.Background(), planNow)
	if err != nil {
		t.Fatal(err)
	}
	if p.ChosenBy != "rules" || len(p.Changes) != len(base.Changes) || !strings.Contains(strings.Join(p.Notes, " "), "proxy down") {
		t.Errorf("fallback plan = %s, %d changes, notes %v", p.ChosenBy, len(p.Changes), p.Notes)
	}
	if _, err := s.plan(context.Background(), planNow.Add(time.Minute)); err != nil || r.calls != 1 {
		t.Errorf("a failure is retried after an hour, not at once: %d calls", r.calls)
	}
	s.planCache = cache[*Plan]{}
	if _, err := s.plan(context.Background(), planNow.Add(2*time.Hour)); err != nil || r.calls != 2 {
		t.Errorf("want a retry after an hour, got %d calls", r.calls)
	}
}

func TestUnknownPicksFallBackToRules(t *testing.T) {
	r := &fakeRanker{subjects: []string{"Nothing Real"}}
	s, _ := rankedServer(t, r, 4)
	p, err := s.plan(context.Background(), planNow)
	if err != nil {
		t.Fatal(err)
	}
	if p.ChosenBy != "rules" || !strings.Contains(strings.Join(p.Notes, " "), "no known option") {
		t.Errorf("chosen by %s, notes %v", p.ChosenBy, p.Notes)
	}
}

func TestRankPromptHoldsNoPaycheckSource(t *testing.T) {
	r := &fakeRanker{subjects: []string{"Old Town Water"}}
	s, _ := rankedServer(t, r, 4)
	if _, err := s.plan(context.Background(), planNow); err != nil {
		t.Fatal(err)
	}
	if len(r.prompts) != 1 || strings.Contains(r.prompts[0], "ACME PAYROLL") || !strings.Contains(r.prompts[0], `"monthly_history"`) {
		t.Errorf("prompt = %.300s", r.prompts)
	}
}

func TestPlanPageShowsTheModelsReasons(t *testing.T) {
	r := &fakeRanker{subjects: []string{"Old Town Water"}}
	s, _ := rankedServer(t, r, 4)
	h := router(s, http.NotFoundHandler())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	for _, want := range []string{"Eat out less.", "Because Old Town Water.", "chosen by fake-model"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestRankerNeedsTheProxy(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "sk-test")
	t.Setenv("GOOGLE_GEMINI_BASE_URL", "")
	if _, _, err := rankerFromEnv(context.Background()); err == nil {
		t.Error("a key without the proxy must be refused")
	}
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("ACNTNG_AI_MAX_CALLS", "2")
	r, n, err := rankerFromEnv(context.Background())
	if err != nil || r != nil || n != 2 {
		t.Errorf("no key: ranker %v, calls %d, err %v", r, n, err)
	}
}

func option(t *testing.T, p *Plan, title string) Option {
	t.Helper()
	for _, o := range p.Options {
		if o.Title == title {
			return o
		}
	}
	t.Fatalf("no option %q", title)
	return Option{}
}

func TestMerchantSavingMatchesTheCategoryMedian(t *testing.T) {
	p := buildFixturePlan(t, planFixture(), planOpts(1000))
	// Restaurants is all Noodle Cart: lately 700, 700 and 600 scaled, a
	// median of 700. Halved, the median is 350.
	if got := option(t, p, "Halve Noodle Cart").Saving; got != 350 {
		t.Errorf("halving saves %v, want 350", got)
	}
}

func TestCategoryThenMerchantIsCountedOnce(t *testing.T) {
	r := &fakeRanker{subjects: []string{"Restaurants", "Halve Noodle Cart"}}
	s, _ := rankedServer(t, r, 4)
	p, err := s.plan(context.Background(), planNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 1 || p.Changes[0].Category != "Restaurants" || p.Changes[0].Saving != 300 {
		t.Errorf("changes = %+v, want only the category's 300", p.Changes)
	}
}

func TestUnusableAnswerIsRetriedAfterAnHour(t *testing.T) {
	r := &fakeRanker{subjects: []string{"Nothing Real"}}
	s, _ := rankedServer(t, r, 4)
	for _, at := range []time.Duration{0, time.Minute} {
		if _, err := s.plan(context.Background(), planNow.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	if r.calls != 1 {
		t.Fatalf("calls = %d, want 1 within the hour", r.calls)
	}
	s.planCache = cache[*Plan]{}
	if _, err := s.plan(context.Background(), planNow.Add(2*time.Hour)); err != nil || r.calls != 2 {
		t.Errorf("an unusable answer should be retried after an hour: %d calls", r.calls)
	}
}

func TestRankPromptNamesEachOptionsCategory(t *testing.T) {
	p := buildFixturePlan(t, planFixture(), planOpts(1000))
	prompt, err := rankPrompt(p, "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"subject":"Noodle Cart","category":"Restaurants"`) {
		t.Errorf("prompt does not tie Noodle Cart to Restaurants: %.400s", prompt)
	}
}
