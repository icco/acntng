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
}

func (f *fakeAdviser) Advise(context.Context, *Plan) (*Advice, error) {
	f.calls++
	if f.err != nil {
		return &Advice{Model: "fake-model", TokensIn: 10}, f.err
	}
	return &Advice{
		Summary:     "Restaurants are the lever.",
		Model:       "fake-model",
		TokensIn:    900,
		TokensOut:   200,
		Suggestions: []AISuggestion{{Title: "Cook at home twice a week", Action: "Replace two dinners out.", MonthlySavings: 120, Why: "Restaurants ran over."}},
	}, nil
}

func TestCoachPaysForAMonthOnce(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	adviser := &fakeAdviser{}
	coach := &Coach{Adviser: adviser, Store: &MemoryStore{}, MaxCalls: 3}
	ctx := context.Background()

	for range 3 {
		a, err := coach.For(ctx, p, false, planNow)
		if err != nil || a == nil || a.Month != "2026-10" || a.TokensOut != 200 {
			t.Fatalf("advice = %+v, err = %v", a, err)
		}
	}
	if adviser.calls != 1 {
		t.Errorf("adviser calls = %d, want 1: later views must read the store", adviser.calls)
	}
}

func TestCoachEnforcesTheMonthlyBudget(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	adviser := &fakeAdviser{}
	coach := &Coach{Adviser: adviser, Store: &MemoryStore{}, MaxCalls: 2}
	ctx := context.Background()

	for range 2 {
		if _, err := coach.For(ctx, p, true, planNow); err != nil {
			t.Fatalf("refresh inside the budget: %v", err)
		}
	}
	a, err := coach.For(ctx, p, true, planNow)
	if !errors.Is(err, ErrAdviceBudget) {
		t.Fatalf("err = %v, want ErrAdviceBudget", err)
	}
	if a == nil || adviser.calls != 2 {
		t.Errorf("want the stored advice back and no third call; advice %v, calls %d", a, adviser.calls)
	}

	// The budget is per calendar month.
	if _, err := coach.For(ctx, p, true, planNow.AddDate(0, 1, 0)); err != nil {
		t.Errorf("next month should have a fresh budget: %v", err)
	}
}

func TestCoachCountsFailedCalls(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	adviser := &fakeAdviser{err: errors.New("model unavailable")}
	coach := &Coach{Adviser: adviser, Store: &MemoryStore{}, MaxCalls: 2}
	ctx := context.Background()

	for range 5 {
		if a, err := coach.For(ctx, p, false, planNow); err == nil || a != nil {
			t.Fatalf("want an error and no advice, got %+v, %v", a, err)
		}
	}
	if adviser.calls != 2 {
		t.Errorf("adviser calls = %d, want 2: a failing model must not be retried on every page view", adviser.calls)
	}
}

func TestCoachSkipsTheModelWithNothingToSay(t *testing.T) {
	adviser := &fakeAdviser{}
	coach := &Coach{Adviser: adviser, Store: &MemoryStore{}, MaxCalls: 2}

	a, err := coach.For(context.Background(), &Plan{Month: "2026-10"}, false, planNow)
	if a != nil || err != nil || adviser.calls != 0 {
		t.Errorf("advice %v, err %v, calls %d; want nothing and no call", a, err, adviser.calls)
	}

	var none *Coach
	if a, err := none.For(context.Background(), &Plan{}, false, planNow); a != nil || err != nil {
		t.Errorf("a nil coach should be a no-op, got %v, %v", a, err)
	}
}

func TestAdvicePromptSendsAggregatesOnly(t *testing.T) {
	p := buildFixturePlan(t, planFixture())
	prompt, err := advicePrompt(p)
	if err != nil {
		t.Fatalf("advicePrompt: %v", err)
	}
	for _, want := range []string{`"paycheck_normal":6000`, `"name":"Restaurants"`, "Noodle Cart", "Streamflix"} {
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

	if a, err := s.Latest(ctx, "2026-10"); a != nil || err != nil {
		t.Fatalf("empty store: %v, %v", a, err)
	}

	full := &Advice{Month: "2026-09", Summary: "other month", Model: "m", TokensIn: 1, TokensOut: 2, GeneratedAt: at,
		Suggestions: []AISuggestion{{Title: "t", Action: "a", MonthlySavings: 12.5, Why: "w"}}}
	for _, step := range []struct {
		month   string
		advice  *Advice
		failure string
		at      time.Time
	}{
		{"2026-10", &Advice{Month: "2026-10", Summary: "first"}, "", at},
		{"2026-10", &Advice{Month: "2026-10", Summary: "second"}, "", at.Add(time.Hour)},
		{"2026-10", &Advice{Model: "m"}, "model unavailable", at.Add(2 * time.Hour)},
		{"2026-09", full, "", at.Add(3 * time.Hour)},
	} {
		if err := s.Record(ctx, step.month, step.advice, step.failure, step.at); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	got, err := s.Latest(ctx, "2026-10")
	if err != nil || got == nil || got.Summary != "second" {
		t.Fatalf("latest = %+v, %v; want the newest success, not the failure after it", got, err)
	}
	got, err = s.Latest(ctx, "2026-09")
	if err != nil || got == nil {
		t.Fatalf("latest for 2026-09: %v, %v", got, err)
	}
	if got.Summary != "other month" || got.TokensOut != 2 || !got.GeneratedAt.Equal(at) ||
		len(got.Suggestions) != 1 || got.Suggestions[0].MonthlySavings != 12.5 {
		t.Errorf("advice did not round-trip: %+v", got)
	}
	if a, err := s.Latest(ctx, "2026-08"); a != nil || err != nil {
		t.Errorf("another month: %v, %v", a, err)
	}

	for since, want := range map[time.Time]int{
		at:                       4,
		at.Add(90 * time.Minute): 2,
		at.AddDate(0, 1, 0):      0,
	} {
		if n, err := s.CountSince(ctx, since); err != nil || n != want {
			t.Errorf("count since %s = %d, %v; want %d", since, n, err, want)
		}
	}
}

func TestMemoryStore(t *testing.T) {
	storeContract(t, &MemoryStore{})
}
