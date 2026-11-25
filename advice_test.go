package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeAdviser struct {
	calls int
	err   error
	// cancel, when set, is called mid-request to mimic a dropped connection.
	cancel context.CancelFunc
}

func (f *fakeAdviser) Advise(_ context.Context, p *Plan) (*Advice, error) {
	f.calls++
	if f.cancel != nil {
		f.cancel()
	}
	if f.err != nil {
		return &Advice{Model: "fake-model", TokensIn: 10}, f.err
	}
	a := &Advice{Model: "fake-model", TokensIn: 900, TokensOut: 200}
	ans := modelAnswer{Summary: "Restaurants are the lever."}
	ans.Suggestions = append(ans.Suggestions, struct {
		ID     int    `json:"id"`
		Action string `json:"action"`
		Why    string `json:"why"`
	}{ID: 0, Action: "Cook at home twice a week.", Why: "Corner Bistro alone was $680."})
	applyAnswer(p, ans, a)
	return a, nil
}

func TestCoachStoredNeverCallsTheModel(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	adviser := &fakeAdviser{}
	coach := &Coach{Adviser: adviser, Store: &MemoryStore{}, MaxCalls: 3}
	ctx := context.Background()

	if a, err := coach.Stored(ctx, p.Month); a != nil || err != nil {
		t.Fatalf("stored before generating = %v, %v", a, err)
	}
	if adviser.calls != 0 {
		t.Fatalf("reading stored advice called the model %d times", adviser.calls)
	}

	a, err := coach.Generate(ctx, p, planNow)
	if err != nil || a == nil || a.Month != "2026-10" || a.TokensOut != 200 {
		t.Fatalf("generate = %+v, %v", a, err)
	}
	for range 3 {
		got, err := coach.Stored(ctx, p.Month)
		if err != nil || got == nil || got.Summary != a.Summary {
			t.Fatalf("stored = %+v, %v", got, err)
		}
	}
	if adviser.calls != 1 {
		t.Errorf("adviser calls = %d, want 1", adviser.calls)
	}
}

func TestCoachEnforcesTheMonthlyBudget(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	adviser := &fakeAdviser{}
	coach := &Coach{Adviser: adviser, Store: &MemoryStore{}, MaxCalls: 2}
	ctx := context.Background()

	for range 2 {
		if _, err := coach.Generate(ctx, p, planNow); err != nil {
			t.Fatalf("generate inside the budget: %v", err)
		}
	}
	if _, err := coach.Generate(ctx, p, planNow); !errors.Is(err, ErrAdviceBudget) {
		t.Fatalf("err = %v, want ErrAdviceBudget", err)
	}
	if adviser.calls != 2 {
		t.Errorf("adviser calls = %d, want 2", adviser.calls)
	}

	// The budget is per calendar month.
	if _, err := coach.Generate(ctx, p, planNow.AddDate(0, 1, 0)); err != nil {
		t.Errorf("next month should have a fresh budget: %v", err)
	}
}

func TestCoachCountsFailedCalls(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	adviser := &fakeAdviser{err: errors.New("model unavailable")}
	coach := &Coach{Adviser: adviser, Store: &MemoryStore{}, MaxCalls: 2}
	ctx := context.Background()

	for range 5 {
		if a, err := coach.Generate(ctx, p, planNow); err == nil || a != nil {
			t.Fatalf("want an error and no advice, got %+v, %v", a, err)
		}
	}
	if adviser.calls != 2 {
		t.Errorf("adviser calls = %d, want 2: failures count against the budget", adviser.calls)
	}
	if a, err := coach.Stored(ctx, p.Month); a != nil || err != nil {
		t.Errorf("a failed call must not be served as advice: %+v, %v", a, err)
	}
}

func TestCoachCountsACallWhoseRequestWasCancelled(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	ctx, cancel := context.WithCancel(context.Background())
	adviser := &fakeAdviser{cancel: cancel}
	store := &MemoryStore{}
	coach := &Coach{Adviser: adviser, Store: store, MaxCalls: 1}

	if _, err := coach.Generate(ctx, p, planNow); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if a, err := coach.Stored(context.Background(), p.Month); a == nil || err != nil {
		t.Errorf("the advice paid for should be kept: %v, %v", a, err)
	}
	if _, err := coach.Generate(context.Background(), p, planNow); !errors.Is(err, ErrAdviceBudget) {
		t.Errorf("err = %v, want the cancelled call to have used the budget", err)
	}
}

func TestCoachSkipsTheModelWithNothingToSay(t *testing.T) {
	adviser := &fakeAdviser{}
	coach := &Coach{Adviser: adviser, Store: &MemoryStore{}, MaxCalls: 2}

	a, err := coach.Generate(context.Background(), &Plan{Month: "2026-10"}, planNow)
	if a != nil || !errors.Is(err, ErrNothingToAdvise) || adviser.calls != 0 {
		t.Errorf("advice %v, err %v, calls %d; want ErrNothingToAdvise and no call", a, err, adviser.calls)
	}

	var none *Coach
	if a, err := none.Generate(context.Background(), &Plan{}, planNow); a != nil || err != nil {
		t.Errorf("a nil coach should be a no-op, got %v, %v", a, err)
	}
	if a, err := none.Stored(context.Background(), "2026-10"); a != nil || err != nil {
		t.Errorf("a nil coach should be a no-op, got %v, %v", a, err)
	}
}

func TestApplyAnswerTakesFiguresFromTheRules(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	over := suggestion(p, KindOverNormal, "Restaurants")
	id := -1
	for i := range p.Suggestions {
		if &p.Suggestions[i] == over {
			id = i
		}
	}

	type pick = struct {
		ID     int    `json:"id"`
		Action string `json:"action"`
		Why    string `json:"why"`
	}
	ans := modelAnswer{Summary: "s", Suggestions: []pick{
		{ID: id, Action: "Eat in.", Why: "It ran over."},
		{ID: id, Action: "Again.", Why: "Duplicate."},
		{ID: 99, Action: "Sell the car.", Why: "Invented."},
		{ID: -1, Action: "Nope.", Why: "Invented."},
	}}

	a := &Advice{}
	applyAnswer(p, ans, a)
	if len(a.Suggestions) != 1 {
		t.Fatalf("suggestions = %+v, want only the one known, unrepeated candidate", a.Suggestions)
	}
	got := a.Suggestions[0]
	if got.Title != over.Title || got.Category != "Restaurants" || got.MonthlySavings == nil || *got.MonthlySavings != 350 {
		t.Errorf("title, category and savings must come from the rule, got %+v", got)
	}
	if got.Action != "Eat in." {
		t.Errorf("action = %q", got.Action)
	}
}

func TestAdvicePromptSendsAggregatesOnly(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	prompt, err := advicePrompt(p)
	if err != nil {
		t.Fatalf("advicePrompt: %v", err)
	}
	for _, want := range []string{`"paycheck_normal":6000`, `"name":"Restaurants"`, "Noodle Cart", "Streamflix", `"candidates":[{"id":0`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %s", want)
		}
	}
	for _, leak := range []string{"ACME", "LOVELACE", "history"} {
		if strings.Contains(prompt, leak) {
			t.Errorf("prompt should not contain %q", leak)
		}
	}
	if len(prompt) > 6000 {
		t.Errorf("prompt is %d bytes; keep it small, it is paid for per token", len(prompt))
	}
}

// storeContract is what the coach relies on from any store.
func storeContract(t *testing.T, s AdviceStore) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	since := monthStart(at)

	if a, err := s.Latest(ctx, "2026-10"); a != nil || err != nil {
		t.Fatalf("empty store: %v, %v", a, err)
	}

	reserve := func(month string, at time.Time) uint {
		t.Helper()
		id, ok, err := s.Reserve(ctx, month, since, 4, at)
		if err != nil || !ok {
			t.Fatalf("reserve %s: ok %v, err %v", month, ok, err)
		}
		return id
	}
	finish := func(id uint, a *Advice, failure string) {
		t.Helper()
		if err := s.Finish(ctx, id, a, failure); err != nil {
			t.Fatalf("finish %d: %v", id, err)
		}
	}

	first := reserve("2026-10", at)
	if a, err := s.Latest(ctx, "2026-10"); a != nil || err != nil {
		t.Fatalf("a reserved, unsettled attempt is not advice: %v, %v", a, err)
	}
	finish(first, &Advice{Month: "2026-10", Summary: "first"}, "")
	finish(reserve("2026-10", at.Add(time.Hour)), &Advice{Month: "2026-10", Summary: "second"}, "")
	finish(reserve("2026-10", at.Add(2*time.Hour)), &Advice{Model: "m"}, "model unavailable")

	full := &Advice{Month: "2026-09", Summary: "other month", Model: "m", TokensIn: 1, TokensOut: 2, GeneratedAt: at,
		Suggestions: []AISuggestion{{Title: "t", Action: "a", MonthlySavings: new(12.5), Why: "w"}}}
	pending := reserve("2026-09", at.Add(3*time.Hour))

	// Four attempts this month, one still pending: the budget is spent.
	if _, ok, err := s.Reserve(ctx, "2026-10", since, 4, at.Add(4*time.Hour)); ok || err != nil {
		t.Errorf("fifth reserve: ok %v, err %v; want refused", ok, err)
	}
	// A new calendar month starts a new count.
	if _, ok, err := s.Reserve(ctx, "2026-10", since.AddDate(0, 1, 0), 1, at.AddDate(0, 1, 0)); !ok || err != nil {
		t.Errorf("reserve next month: ok %v, err %v", ok, err)
	}
	if _, ok, err := s.Reserve(ctx, "2026-10", since, 0, at); ok || err != nil {
		t.Errorf("a zero limit must refuse: ok %v, err %v", ok, err)
	}
	finish(pending, full, "")

	got, err := s.Latest(ctx, "2026-10")
	if err != nil || got == nil || got.Summary != "second" {
		t.Fatalf("latest = %+v, %v; want the newest success, not the failure after it", got, err)
	}
	got, err = s.Latest(ctx, "2026-09")
	if err != nil || got == nil {
		t.Fatalf("latest for 2026-09: %v, %v", got, err)
	}
	if got.Summary != "other month" || got.TokensOut != 2 || !got.GeneratedAt.Equal(at) ||
		len(got.Suggestions) != 1 || *got.Suggestions[0].MonthlySavings != 12.5 {
		t.Errorf("advice did not round-trip: %+v", got)
	}
	if a, err := s.Latest(ctx, "2026-08"); a != nil || err != nil {
		t.Errorf("another month: %v, %v", a, err)
	}
	if err := s.Finish(ctx, 999999, &Advice{}, ""); err == nil {
		t.Errorf("finishing an unknown attempt should fail")
	}
}

func TestMemoryStore(t *testing.T) {
	storeContract(t, &MemoryStore{})
}
