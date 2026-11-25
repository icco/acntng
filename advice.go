package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"go.icco.me/gutil/vertex"
	"google.golang.org/genai"
)

const (
	// defaultAIModel is the cheapest tier: the model only rewords and ranks
	// figures that are already computed.
	defaultAIModel = "gemini-3.5-flash-lite"
	// defaultAICallsPerMonth bounds spend however often the page is loaded.
	defaultAICallsPerMonth = 4
	maxAISuggestions       = 5
)

const adviceSystem = `You are a frugal personal finance coach reviewing one household's month.
You get JSON: paycheck income, spending per category against that category's normal (median) month, top merchants, recurring charges, and numbered candidate suggestions.
Write a plan for next month that saves money.
- Pick the few candidates that matter most, by their id. Each id at most once.
- For each, write one concrete action for next month and one sentence on why, naming the merchants or amounts in the data that make it specific.
- Use only the figures given. Never invent merchants, amounts, or categories.
- Leave categories marked fixed alone.
- Keep summary to two sentences.`

// AISuggestion is a rule-based suggestion the model chose and put into words.
// Title, Category and MonthlySavings are copied from the rule, never written
// by the model, so no dollar figure on the page is a model's guess.
type AISuggestion struct {
	Title          string   `json:"title"`
	Action         string   `json:"action"`
	Category       string   `json:"category,omitempty"`
	MonthlySavings *float64 `json:"monthly_savings"`
	Why            string   `json:"why"`
}

// Advice is the model's read of a plan, plus what it cost to produce.
type Advice struct {
	Month       string         `json:"month"`
	Summary     string         `json:"summary"`
	Suggestions []AISuggestion `json:"suggestions"`
	Model       string         `json:"model"`
	TokensIn    int            `json:"tokens_in"`
	TokensOut   int            `json:"tokens_out"`
	GeneratedAt time.Time      `json:"generated_at"`
}

// Adviser turns a plan into advice. Usage is reported even on failure, since a
// call that returned nothing useful was still billed.
type Adviser interface {
	Advise(ctx context.Context, p *Plan) (*Advice, error)
}

// AdviceStore remembers advice so a month is paid for once.
type AdviceStore interface {
	// Latest returns the newest successful advice for a review month, or nil.
	Latest(ctx context.Context, month string) (*Advice, error)
	// Reserve records an attempt before the model is called, so the call is
	// counted whatever happens next. It reports false, recording nothing,
	// when limit attempts already exist at or after since. Check and insert
	// are atomic.
	Reserve(ctx context.Context, month string, since time.Time, limit int, at time.Time) (id uint, ok bool, err error)
	// Finish fills in a reserved attempt. A non-empty failure keeps it out of
	// Latest.
	Finish(ctx context.Context, id uint, a *Advice, failure string) error
}

// finishTimeout bounds the write that records how a model call ended.
const finishTimeout = 10 * time.Second

// Coach hands out advice within a monthly call budget.
type Coach struct {
	Adviser Adviser
	Store   AdviceStore
	// MaxCalls is the most model calls allowed per calendar month.
	MaxCalls int

	mu sync.Mutex
}

// ErrAdviceBudget means the month's model calls are used up.
var ErrAdviceBudget = errors.New("monthly AI call budget is used up")

// ErrNothingToAdvise means the plan gives a model nothing to work with.
var ErrNothingToAdvise = errors.New("no baseline or suggestions to advise on")

// Stored returns the saved advice for a review month, or nil. It never calls
// the model, so it is safe on every page view.
func (c *Coach) Stored(ctx context.Context, month string) (*Advice, error) {
	if c == nil {
		return nil, nil
	}
	a, err := c.Store.Latest(ctx, month)
	if err != nil {
		return nil, fmt.Errorf("read stored advice: %w", err)
	}
	return a, nil
}

// Generate spends one model call on advice for the plan. The attempt is
// recorded before the call and settled after it on a context of its own, so
// neither a failure nor a dropped connection leaves a call uncounted.
func (c *Coach) Generate(ctx context.Context, p *Plan, now time.Time) (*Advice, error) {
	if c == nil {
		return nil, nil
	}
	if len(p.BaselineMonths) == 0 || len(p.Suggestions) == 0 {
		return nil, ErrNothingToAdvise
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	id, ok, err := c.Store.Reserve(ctx, p.Month, monthStart(now), c.MaxCalls, now)
	if err != nil {
		return nil, fmt.Errorf("reserve advice call: %w", err)
	}
	if !ok {
		return nil, ErrAdviceBudget
	}

	advice, adviseErr := c.Adviser.Advise(ctx, p)
	if advice == nil {
		advice = &Advice{}
	}
	advice.Month = p.Month
	advice.GeneratedAt = now.UTC()

	failure := ""
	if adviseErr != nil {
		failure = adviseErr.Error()
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err := c.Store.Finish(finishCtx, id, advice, failure); err != nil {
		return nil, errors.Join(adviseErr, fmt.Errorf("record advice call: %w", err))
	}
	if adviseErr != nil {
		return nil, adviseErr
	}
	return advice, nil
}

// promptCategory is a CategoryLine without the history, which the model does
// not need once normal and low are computed.
type promptCategory struct {
	Name   string  `json:"name"`
	Fixed  bool    `json:"fixed"`
	Normal float64 `json:"normal"`
	Low    float64 `json:"low"`
	Spent  float64 `json:"spent"`
}

// promptCandidate is a rule-based suggestion the model may pick by ID.
type promptCandidate struct {
	ID int `json:"id"`
	Suggestion
}

type promptInput struct {
	Month      string            `json:"month"`
	PlanFor    string            `json:"plan_for"`
	Totals     PlanTotals        `json:"totals"`
	Categories []promptCategory  `json:"categories"`
	Merchants  []MerchantLine    `json:"top_merchants"`
	Recurring  []RecurringCharge `json:"recurring_charges"`
	Candidates []promptCandidate `json:"candidates"`
}

// advicePrompt is the whole of what leaves for the model: aggregates and
// merchant names. No transactions, account names, or paycheck sources.
func advicePrompt(p *Plan) (string, error) {
	in := promptInput{
		Month:     p.Month,
		PlanFor:   p.PlanFor,
		Totals:    p.Totals,
		Merchants: p.Merchants,
		Recurring: p.Recurring,
	}
	for _, c := range p.Categories {
		in.Categories = append(in.Categories, promptCategory{
			Name: c.Name, Fixed: c.Fixed, Normal: c.Normal, Low: c.Low, Spent: c.Review,
		})
	}
	for i, s := range p.Suggestions {
		in.Candidates = append(in.Candidates, promptCandidate{ID: i, Suggestion: s})
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("encode advice prompt: %w", err)
	}
	return string(b), nil
}

func adviceSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"summary": {Type: genai.TypeString},
			"suggestions": {
				Type:     genai.TypeArray,
				MaxItems: genai.Ptr[int64](maxAISuggestions),
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"id":     {Type: genai.TypeInteger},
						"action": {Type: genai.TypeString},
						"why":    {Type: genai.TypeString},
					},
					Required: []string{"id", "action", "why"},
				},
			},
		},
		Required: []string{"summary", "suggestions"},
	}
}

// modelAnswer is what the model is allowed to say: which candidates, and how
// to act on them.
type modelAnswer struct {
	Summary     string `json:"summary"`
	Suggestions []struct {
		ID     int    `json:"id"`
		Action string `json:"action"`
		Why    string `json:"why"`
	} `json:"suggestions"`
}

// applyAnswer joins the model's picks to the rule-based suggestions they refer
// to. Unknown and repeated IDs are dropped.
func applyAnswer(p *Plan, ans modelAnswer, into *Advice) {
	into.Summary = ans.Summary
	into.Suggestions = []AISuggestion{}
	seen := map[int]bool{}
	for _, pick := range ans.Suggestions {
		if pick.ID < 0 || pick.ID >= len(p.Suggestions) || seen[pick.ID] || len(into.Suggestions) == maxAISuggestions {
			continue
		}
		seen[pick.ID] = true
		rule := p.Suggestions[pick.ID]
		into.Suggestions = append(into.Suggestions, AISuggestion{
			Title:          rule.Title,
			Action:         pick.Action,
			Category:       rule.Category,
			MonthlySavings: rule.MonthlySavings,
			Why:            pick.Why,
		})
	}
}

// GeminiAdviser asks Gemini for advice in a single request.
type GeminiAdviser struct {
	client *vertex.Client
}

// NewGeminiAdviser builds an adviser that reaches Gemini through the LiteLLM
// proxy: GEMINI_API_KEY is the service's LiteLLM virtual key and
// GOOGLE_GEMINI_BASE_URL the proxy. It returns nil when no key is set, which
// leaves the service on rule-based suggestions.
//
// A key without a proxy URL is an error rather than a fallback to Google's
// own endpoint: the key means nothing there, and sending it would leak it.
func NewGeminiAdviser(ctx context.Context, model string) (*GeminiAdviser, error) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil, nil
	}
	baseURL := os.Getenv("GOOGLE_GEMINI_BASE_URL")
	if baseURL == "" {
		return nil, errors.New("GEMINI_API_KEY is set without GOOGLE_GEMINI_BASE_URL; point it at the LiteLLM proxy")
	}

	client, err := vertex.New(ctx, vertex.Config{APIKey: key, BaseURL: baseURL, Model: model})
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}
	return &GeminiAdviser{client: client}, nil
}

// Advise sends one JSON-constrained request with thinking off.
func (g *GeminiAdviser) Advise(ctx context.Context, p *Plan) (*Advice, error) {
	prompt, err := advicePrompt(p)
	if err != nil {
		return nil, err
	}

	advice := &Advice{Model: g.client.Model()}
	var ans modelAnswer
	resp, err := g.client.GenerateJSON(ctx, vertex.Request{
		System:         adviceSystem,
		Parts:          vertex.Text(prompt),
		Schema:         adviceSchema(),
		ThinkingBudget: vertex.NoThinking(),
		Temperature:    vertex.Temperature(0.2),
	}, &ans)
	if resp != nil {
		advice.TokensIn, advice.TokensOut = resp.Usage.In, resp.Usage.Out
	}
	if err != nil {
		return advice, fmt.Errorf("gemini advice: %w", err)
	}
	applyAnswer(p, ans, advice)
	if len(advice.Suggestions) == 0 {
		return advice, errors.New("gemini advice: the model picked no known candidate")
	}
	return advice, nil
}
