package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	defaultAIModel = "gemini-3.5-flash-lite"
	// defaultAICallsPerDay bounds spend however often the facts change.
	defaultAICallsPerDay = 4
	maxPicks             = 7
	maxWhy               = 400
	// rankTimeout leaves room inside the server's 30 second write timeout
	// for the rules to answer when the model is slow.
	rankTimeout = 20 * time.Second
	// rankRetryAfter is how long a failed ranking is remembered before the
	// model is asked again.
	rankRetryAfter = time.Hour
)

const rankSystem = `You decide what one household should change next month to spend less.
You get JSON facts: income, bills, spending per category with twelve months of history, one-off purchases, the savings target and the gap to it, and numbered options. Each option is one lever with its saving already worked out.
Pick the options that matter most, most important first, by id.
- Favour money that buys nothing (fees, interest, bills for things no longer used), habits that grew and will repeat, and large categories the household controls.
- Pass over spending that was a one-off and will not repeat, and essentials that cannot shrink.
- At most one option per subject. A merchant option and its own category option count as the same money; pick one.
- Pick enough to close the gap when the options allow it, between 3 and 7 in all.
- For each pick, write why in one or two plain sentences grounded in the facts. Do not state any figure that is not in the facts.
- summary is two sentences on what most needs to change and why.`

// Pick is one option the model chose and its reason.
type Pick struct {
	ID  int    `json:"id"`
	Why string `json:"why"`
}

// Ranking is the model's choice of options for a plan.
type Ranking struct {
	Summary   string `json:"summary"`
	Picks     []Pick `json:"picks"`
	Model     string `json:"model"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
}

// Ranker picks the options that matter from a prompt of facts.
type Ranker interface {
	Rank(ctx context.Context, prompt string) (*Ranking, error)
}

type rankFacts struct {
	PlanFor  string         `json:"plan_for"`
	Since    string         `json:"current_setup_since,omitempty"`
	Lately   []string       `json:"lately_months"`
	Target   float64        `json:"savings_target"`
	Income   float64        `json:"income"`
	Bills    []rankBill     `json:"bills"`
	Spending []rankCategory `json:"spending"`
	Left     float64        `json:"left_over_if_nothing_changes"`
	Gap      float64        `json:"gap_to_target"`
	OneOffs  []OneOff       `json:"one_offs_left_out"`
	Stopped  []StoppedBill  `json:"bills_no_longer_due"`
	Notes    []string       `json:"notes,omitempty"`
	Options  []rankOption   `json:"options"`
}

type rankBill struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Monthly  float64 `json:"monthly"`
	Debt     bool    `json:"debt"`
	Since    string  `json:"first_seen,omitempty"`
}

type rankCategory struct {
	Name    string    `json:"name"`
	Usual   float64   `json:"usual"`
	Lately  float64   `json:"lately"`
	Low     float64   `json:"leanest_quarter"`
	History []float64 `json:"monthly_history"`
}

type rankOption struct {
	ID       int     `json:"id"`
	Kind     string  `json:"kind"`
	Subject  string  `json:"subject"`
	Category string  `json:"category,omitempty"`
	Action   string  `json:"action"`
	Facts    string  `json:"facts"`
	Saving   float64 `json:"monthly_saving"`
}

// rankPrompt is everything sent to the model: totals, category and merchant
// figures and payee names. No transactions, accounts or paycheck sources.
func rankPrompt(p *Plan, since string) (string, error) {
	f := rankFacts{
		PlanFor: p.PlanFor, Since: since, Lately: p.Lately, Target: p.Totals.Target,
		Income: p.Totals.Income, Left: p.Totals.Unchanged, Gap: round2(max(p.Totals.Target-p.Totals.Unchanged, 0)),
		OneOffs: p.OneOffs, Stopped: p.Stopped, Notes: p.Notes,
	}
	for _, b := range p.Bills {
		f.Bills = append(f.Bills, rankBill{Name: b.Name, Category: b.Category, Monthly: b.Monthly, Debt: b.Debt, Since: b.FirstSeen})
	}
	for _, c := range p.Caps {
		f.Spending = append(f.Spending, rankCategory{Name: c.Category, Usual: c.Usual, Lately: c.Lately, Low: c.Low, History: c.History})
	}
	for _, o := range p.Options {
		f.Options = append(f.Options, rankOption{ID: o.ID, Kind: o.Kind, Subject: o.Subject, Category: o.Category, Action: o.Title, Facts: o.Detail, Saving: o.Saving})
	}
	b, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("encode rank prompt: %w", err)
	}
	return string(b), nil
}

// GeminiRanker asks Gemini, through the LiteLLM proxy, in one request.
type GeminiRanker struct {
	client *vertex.Client
}

// NewGeminiRanker builds a ranker from GEMINI_API_KEY, this service's LiteLLM
// virtual key, and GOOGLE_GEMINI_BASE_URL, the proxy. It returns nil without
// a key. A key without a proxy is an error, so the key never reaches Google.
func NewGeminiRanker(ctx context.Context, model string) (*GeminiRanker, error) {
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
	return &GeminiRanker{client: client}, nil
}

// Rank sends one JSON-constrained request with thinking off.
func (g *GeminiRanker) Rank(ctx context.Context, prompt string) (*Ranking, error) {
	var ans struct {
		Summary string `json:"summary"`
		Picks   []Pick `json:"picks"`
	}
	resp, err := g.client.GenerateJSON(ctx, vertex.Request{
		System:         rankSystem,
		Parts:          vertex.Text(prompt),
		Schema:         rankSchema(),
		ThinkingBudget: vertex.NoThinking(),
		Temperature:    vertex.Temperature(0.2),
	}, &ans)
	r := &Ranking{Model: g.client.Model(), Summary: ans.Summary, Picks: ans.Picks}
	if resp != nil {
		r.TokensIn, r.TokensOut = resp.Usage.In, resp.Usage.Out
	}
	if err != nil {
		return r, fmt.Errorf("gemini rank: %w", err)
	}
	return r, nil
}

func rankSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"summary": {Type: genai.TypeString},
			"picks": {
				Type:     genai.TypeArray,
				MinItems: genai.Ptr[int64](1),
				MaxItems: genai.Ptr[int64](maxPicks),
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"id":  {Type: genai.TypeInteger},
						"why": {Type: genai.TypeString},
					},
					Required: []string{"id", "why"},
				},
			},
		},
		Required: []string{"summary", "picks"},
	}
}

// rankCache asks the model at most once per set of facts and at most
// maxCalls times a UTC day, with every call and its outcome kept in store.
type rankCache struct {
	mu       sync.Mutex
	store    RankStore
	maxCalls int
}

// ErrRankBudget means today's model calls are used up.
var ErrRankBudget = errors.New("today's model calls are used up")

// finishTimeout bounds the write that records how a call ended.
const finishTimeout = 10 * time.Second

// get returns the ranking for prompt. check rejects an answer before it is
// kept, so an unusable one is retried after an hour like any failure.
func (c *rankCache) get(ctx context.Context, r Ranker, prompt string, check func(*Ranking) error, now time.Time) (*Ranking, error) {
	sum := sha256.Sum256([]byte(prompt))
	key := hex.EncodeToString(sum[:])

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		c.store = &MemoryStore{}
	}

	got, failure, at, err := c.store.Lookup(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read stored ranking: %w", err)
	}
	if got != nil {
		return got, nil
	}
	if failure != "" && now.Sub(at) < rankRetryAfter {
		return nil, errors.New(failure)
	}

	day := now.UTC().Truncate(24 * time.Hour)
	id, ok, err := c.store.Reserve(ctx, key, day, c.maxCalls, now)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrRankBudget
	}

	// The call is counted already, so it runs to the end even when the
	// request that asked for it goes away.
	bg := context.WithoutCancel(ctx)
	callCtx, cancel := context.WithTimeout(bg, rankTimeout)
	defer cancel()
	ranking, rankErr := r.Rank(callCtx, prompt)
	if rankErr == nil && (ranking == nil || len(ranking.Picks) == 0) {
		rankErr = errors.New("the model picked nothing")
	}
	if rankErr == nil && check != nil {
		rankErr = check(ranking)
	}
	failure = ""
	if rankErr != nil {
		failure = rankErr.Error()
	}

	finishCtx, cancelFinish := context.WithTimeout(bg, finishTimeout)
	defer cancelFinish()
	if err := c.store.Finish(finishCtx, id, ranking, failure); err != nil {
		return nil, errors.Join(rankErr, fmt.Errorf("record ranking: %w", err))
	}
	if rankErr != nil {
		return nil, rankErr
	}
	return ranking, nil
}

// clean trims what the model wrote: unknown and repeated ids go, and each
// reason is cut to length.
func (r *Ranking) clean(p *Plan) []Pick {
	known := map[int]bool{}
	for _, o := range p.Options {
		known[o.ID] = true
	}
	seen := map[int]bool{}
	var out []Pick
	for _, pick := range r.Picks {
		if !known[pick.ID] || seen[pick.ID] || len(out) == maxPicks {
			continue
		}
		seen[pick.ID] = true
		if len(pick.Why) > maxWhy {
			pick.Why = pick.Why[:maxWhy]
		}
		out = append(out, pick)
	}
	return out
}
