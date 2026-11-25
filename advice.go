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
You get JSON: paycheck income, spending per category against that category's normal (median) month, top merchants, recurring charges, and rule-based candidate suggestions.
Write a plan for next month that saves money.
- Use only the figures given. Never invent merchants, amounts, or categories.
- Leave categories marked fixed alone.
- Each suggestion is one concrete action for next month, with a monthly saving in dollars that the figures support.
- Prefer the few changes that save the most. Do not repeat a suggestion.
- Keep summary to two sentences, and each action and reason to one sentence.`

// AISuggestion is one model-written suggestion.
type AISuggestion struct {
	Title          string  `json:"title"`
	Action         string  `json:"action"`
	Category       string  `json:"category,omitempty"`
	MonthlySavings float64 `json:"monthly_savings"`
	Why            string  `json:"why"`
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
	// Record saves an attempt. A failed attempt is saved with its error so it
	// still counts against the cap.
	Record(ctx context.Context, month string, a *Advice, failure string, at time.Time) error
	// CountSince counts attempts, failed ones included.
	CountSince(ctx context.Context, since time.Time) (int, error)
}

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

// For returns stored advice for the plan's month, generating it at most once.
// With refresh set it generates again, still inside the budget. A nil result
// with a nil error means there is no advice and none was attempted.
func (c *Coach) For(ctx context.Context, p *Plan, refresh bool, now time.Time) (*Advice, error) {
	if c == nil || c.Adviser == nil || c.Store == nil {
		return nil, nil
	}

	// One generation at a time, so concurrent first views cannot each pay.
	c.mu.Lock()
	defer c.mu.Unlock()

	stored, err := c.Store.Latest(ctx, p.Month)
	if err != nil {
		return nil, fmt.Errorf("read stored advice: %w", err)
	}
	if stored != nil && !refresh {
		return stored, nil
	}
	// Without a baseline or suggestions there is nothing for a model to add.
	if len(p.BaselineMonths) == 0 || len(p.Suggestions) == 0 {
		return stored, nil
	}

	used, err := c.Store.CountSince(ctx, monthStart(now))
	if err != nil {
		return stored, fmt.Errorf("count advice calls: %w", err)
	}
	if used >= c.MaxCalls {
		return stored, ErrAdviceBudget
	}

	advice, err := c.Adviser.Advise(ctx, p)
	if err != nil {
		if advice == nil {
			advice = &Advice{}
		}
		if recErr := c.Store.Record(ctx, p.Month, advice, err.Error(), now); recErr != nil {
			return stored, errors.Join(err, fmt.Errorf("record failed advice call: %w", recErr))
		}
		return stored, err
	}

	advice.Month = p.Month
	advice.GeneratedAt = now.UTC()
	if err := c.Store.Record(ctx, p.Month, advice, "", now); err != nil {
		return advice, fmt.Errorf("store advice: %w", err)
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

type promptInput struct {
	Month       string            `json:"month"`
	PlanFor     string            `json:"plan_for"`
	Totals      PlanTotals        `json:"totals"`
	Categories  []promptCategory  `json:"categories"`
	Merchants   []MerchantLine    `json:"top_merchants"`
	Recurring   []RecurringCharge `json:"recurring_charges"`
	Suggestions []Suggestion      `json:"candidates"`
}

// advicePrompt is the whole of what leaves for the model: aggregates and
// merchant names. No transactions, account names, or paycheck sources.
func advicePrompt(p *Plan) (string, error) {
	in := promptInput{
		Month:       p.Month,
		PlanFor:     p.PlanFor,
		Totals:      p.Totals,
		Merchants:   p.Merchants,
		Recurring:   p.Recurring,
		Suggestions: p.Suggestions,
	}
	for _, c := range p.Categories {
		in.Categories = append(in.Categories, promptCategory{
			Name: c.Name, Fixed: c.Fixed, Normal: c.Normal, Low: c.Low, Spent: c.Review,
		})
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
						"title":           {Type: genai.TypeString},
						"action":          {Type: genai.TypeString},
						"category":        {Type: genai.TypeString},
						"monthly_savings": {Type: genai.TypeNumber},
						"why":             {Type: genai.TypeString},
					},
					Required: []string{"title", "action", "monthly_savings", "why"},
				},
			},
		},
		Required: []string{"summary", "suggestions"},
	}
}

// GeminiAdviser asks Gemini for advice in a single request.
type GeminiAdviser struct {
	client *vertex.Client
}

// NewGeminiAdviser builds an adviser from the environment: GEMINI_API_KEY for
// the Gemini API, or GOOGLE_CLOUD_PROJECT for Vertex AI. It returns nil when
// neither is set, which leaves the service on rule-based suggestions.
func NewGeminiAdviser(ctx context.Context, model string) (*GeminiAdviser, error) {
	cfg := vertex.Config{Model: model}
	switch key, project := os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_CLOUD_PROJECT"); {
	case key != "":
		cfg.APIKey = key
	case project != "":
		cfg.Project = project
		cfg.Location = os.Getenv("GOOGLE_CLOUD_LOCATION")
	default:
		return nil, nil
	}

	client, err := vertex.New(ctx, cfg)
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
	resp, err := g.client.GenerateJSON(ctx, vertex.Request{
		System:         adviceSystem,
		Parts:          vertex.Text(prompt),
		Schema:         adviceSchema(),
		ThinkingBudget: vertex.NoThinking(),
		Temperature:    vertex.Temperature(0.2),
	}, advice)
	if resp != nil {
		advice.TokensIn, advice.TokensOut = resp.Usage.In, resp.Usage.Out
	}
	if err != nil {
		return advice, fmt.Errorf("gemini advice: %w", err)
	}
	if len(advice.Suggestions) > maxAISuggestions {
		advice.Suggestions = advice.Suggestions[:maxAISuggestions]
	}
	return advice, nil
}
