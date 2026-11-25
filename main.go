package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/unrolled/secure"
	"go.icco.me/gutil/logging"
	"go.icco.me/gutil/render"
	"go.icco.me/gutil/tracing"
	"go.icco.me/lunchmoney"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/zap"
)

// serverName is the otelhttp span/metric scope.
const serverName = "acntng"

// The API is rate limited and balances move at most daily.
const cacheTTL = 5 * time.Minute

func main() {
	// No defers here, so run()'s deferred flush and shutdown always complete.
	os.Exit(run())
}

// getToken reads the Lunch Money API token from common environment variables.
func getToken() string {
	for _, env := range []string{"LUNCHMONEY_TOKEN", "LUNCH_MONEY_KEY", "LUNCHMONEY_API_TOKEN"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}

func run() int {
	cliFlag := flag.Bool("cli", false, "Run in CLI mode and print report directly to stdout")
	monthFlag := flag.String("month", "", "With -debts, the budget month in YYYY-MM format (defaults to the current month)")
	jsonFlag := flag.Bool("json", false, "Output report as JSON in CLI mode")
	debtsFlag := flag.Bool("debts", false, "In CLI mode, print the budget and debt report instead of the plan")
	flag.Parse()

	log := logging.Must(logging.NewLogger(serverName))
	defer logging.Sync(log)

	ctx := context.Background()
	shutdown, err := tracing.Init(ctx, serverName)
	if err != nil {
		log.Errorw("tracing init", zap.Error(err))
		return 1
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(shutdownCtx); err != nil {
			log.Warnw("tracing shutdown", zap.Error(err))
		}
	}()

	token := getToken()
	if token == "" {
		log.Errorw("LUNCHMONEY_TOKEN, LUNCH_MONEY_KEY, or LUNCHMONEY_API_TOKEN is required")
		return 1
	}

	lm, err := lunchmoney.NewClient(token)
	if err != nil {
		log.Errorw("could not create lunchmoney client", zap.Error(err))
		return 1
	}
	if lm.HTTP != nil {
		lm.HTTP.Transport = otelhttp.NewTransport(lm.HTTP.Transport)
	}

	overrides, err := parseOverrides(os.Getenv("ACNTNG_PAYMENT_OVERRIDES"))
	if err != nil {
		log.Errorw("could not parse ACNTNG_PAYMENT_OVERRIDES", zap.Error(err))
		return 1
	}
	if len(overrides) > 0 {
		log.Infow("loaded payment overrides", "count", len(overrides))
	}

	planOpts, err := planOptionsFromEnv()
	if err != nil {
		log.Errorw("could not read plan configuration", zap.Error(err))
		return 1
	}

	if *cliFlag {
		if *debtsFlag {
			return runCLI(context.Background(), lm, overrides, *monthFlag, *jsonFlag)
		}
		return runPlanCLI(ctx, &Server{Log: log, Client: lm, PlanOptions: planOpts}, *jsonFlag)
	}

	// Siblings on mist's shared network reach this container directly, so fail
	// closed in production. See requireSharedKey.
	sharedKey := os.Getenv("ACNTNG_SHARED_KEY")
	if sharedKey == "" {
		if os.Getenv("NAT_ENV") == "production" {
			log.Errorw("ACNTNG_SHARED_KEY is required when NAT_ENV=production")
			return 1
		}
		log.Warnw("ACNTNG_SHARED_KEY is unset; report routes are unauthenticated")
	}

	port := "8080"
	if fromEnv := os.Getenv("PORT"); fromEnv != "" {
		port = fromEnv
	}

	registry := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		log.Errorw("otel prometheus exporter", zap.Error(err))
		return 1
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(mp)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mp.Shutdown(shutdownCtx); err != nil {
			log.Warnw("meter provider shutdown", zap.Error(err))
		}
	}()

	srv := &http.Server{
		Addr: ":" + port,
		Handler: router(&Server{
			Log:         log,
			Client:      lm,
			Overrides:   overrides,
			PlanOptions: planOpts,
			SharedKey:   sharedKey,
			Now:         time.Now,
		}, promhttp.HandlerFor(registry, promhttp.HandlerOpts{})),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Errorw("server shutdown", zap.Error(err))
		}
		close(idle)
	}()

	log.Infow("starting server", "port", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Errorw("server error", zap.Error(err))
		return 1
	}
	<-idle

	return 0
}

// Fetcher is everything acntng needs from the Lunch Money client, so tests can
// supply canned data for both reports.
type Fetcher interface {
	LoanFetcher
	BudgetFetcher
	TransactionFetcher
}

// Server holds the request-scoped dependencies for the HTTP handlers.
type Server struct {
	Log       *zap.SugaredLogger
	Client    Fetcher
	Overrides map[string]float64
	// PlanOptions carries the plan's configuration; Now is set per request.
	PlanOptions PlanOptions
	// SharedKey, when set, is required on report requests.
	SharedKey string
	// Now is injectable so tests can pin the reporting month.
	Now func() time.Time

	loanCache   cache[*Report]
	budgetCache cache[*BudgetReport]
	planCache   cache[*Plan]
}

// clock returns the injected clock, or the real one.
func (s *Server) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// loanReport returns the loan report for these options, from cache when fresh.
func (s *Server) loanReport(ctx context.Context, opts Options, now time.Time) (*Report, error) {
	key := fmt.Sprintf("credit=%t&liabilities=%t", opts.IncludeCredit, opts.IncludeLiabilities)
	if rep, ok := s.loanCache.get(key, now); ok {
		return rep, nil
	}

	rep, err := BuildReport(ctx, s.Client, now, opts)
	if err != nil {
		return nil, err
	}

	s.loanCache.set(key, rep, now)
	return rep, nil
}

// budgetReport returns the budget report for the month containing at, from
// cache when fresh. The cache is keyed by month so browsing between periods
// does not evict the one being compared against.
func (s *Server) budgetReport(ctx context.Context, at, now time.Time) (*BudgetReport, error) {
	key := monthStart(at).Format("2006-01")
	if rep, ok := s.budgetCache.get(key, now); ok {
		return rep, nil
	}

	rep, err := BuildBudgetReport(ctx, s.Client, at)
	if err != nil {
		return nil, err
	}

	s.budgetCache.set(key, rep, now)
	return rep, nil
}

// parseOverrides reads ACNTNG_PAYMENT_OVERRIDES: a JSON object mapping a loan
// ID ("asset:12") to its monthly payment.
func parseOverrides(s string) (map[string]float64, error) {
	if s == "" {
		return nil, nil
	}
	out := map[string]float64{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("expected a JSON object of loan id to monthly payment: %w", err)
	}
	return out, nil
}

func router(s *Server, metrics http.Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Heartbeat("/healthz"))
	r.Use(logging.Middleware(s.Log.Desugar()))
	r.Use(secure.New(secure.Options{
		FrameDeny:          true,
		ContentTypeNosniff: true,
		BrowserXssFilter:   true,
		SSLRedirect:        false,
		IsDevelopment:      os.Getenv("NAT_ENV") != "production",
	}).Handler)

	r.Handle("/metrics", metrics)

	r.Group(func(r chi.Router) {
		r.Use(requireSharedKey(s.SharedKey))
		r.Get("/", s.handlePlanPage)
		r.Get("/plan", s.handlePlan)
		r.Get("/dashboard", s.handleDashboard)
		r.Get("/loans", s.handleLoans)
		r.Get("/budget", s.handleBudget)
	})

	return otelhttp.NewHandler(r, serverName,
		otelhttp.WithFilter(func(req *http.Request) bool {
			return req.URL.Path != "/metrics" && req.URL.Path != "/healthz"
		}),
	)
}

// handleLoans returns the loan report as JSON.
func (s *Server) handleLoans(w http.ResponseWriter, r *http.Request) {
	log := logging.FromContext(r.Context())

	opts, err := optionsFromRequest(r)
	if err != nil {
		render.JSON(log, w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	opts.Overrides = s.Overrides

	rep, err := s.loanReport(r.Context(), opts, s.clock())
	if err != nil {
		log.Errorw("could not build loan report", zap.Error(err))
		render.JSON(log, w, http.StatusBadGateway,
			map[string]string{"error": "could not read from lunch money"})
		return
	}

	render.JSON(log, w, http.StatusOK, rep)
}

// monthFromRequest reads an optional month=YYYY-MM param, defaulting to the
// month containing now.
func monthFromRequest(r *http.Request, now time.Time) (time.Time, error) {
	raw := r.URL.Query().Get("month")
	if raw == "" {
		return now, nil
	}

	at, err := time.Parse("2006-01", raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("month must look like 2006-01, got %q", raw)
	}
	return at, nil
}

// optionsFromRequest reads the params that widen what counts as a loan.
func optionsFromRequest(r *http.Request) (Options, error) {
	var opts Options

	for name, dst := range map[string]*bool{
		"include_credit":      &opts.IncludeCredit,
		"include_liabilities": &opts.IncludeLiabilities,
	} {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			continue
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return opts, fmt.Errorf("%s must be a boolean, got %q", name, raw)
		}
		*dst = v
	}

	return opts, nil
}

// planOptionsFromEnv reads the optional plan settings.
func planOptionsFromEnv() (PlanOptions, error) {
	opts := PlanOptions{
		HistoryMonths: defaultHistoryMonths,
		SavingsTarget: defaultSavingsTarget,
		Fixed:         fixedSet(os.Getenv("ACNTNG_FIXED_CATEGORIES")),
	}

	if raw := os.Getenv("ACNTNG_HISTORY_MONTHS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 36 {
			return opts, fmt.Errorf("ACNTNG_HISTORY_MONTHS must be between 1 and 36, got %q", raw)
		}
		opts.HistoryMonths = n
	}

	if raw := os.Getenv("ACNTNG_SAVINGS_TARGET"); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || f < 0 {
			return opts, fmt.Errorf("ACNTNG_SAVINGS_TARGET must be a non-negative number, got %q", raw)
		}
		opts.SavingsTarget = f
	}

	if raw := os.Getenv("ACNTNG_SINCE"); raw != "" {
		t, err := time.Parse(monthKey, raw)
		if err != nil {
			return opts, fmt.Errorf("ACNTNG_SINCE must look like 2006-01, got %q", raw)
		}
		opts.Since = t
	}

	pattern := defaultPaycheckPattern
	if raw := os.Getenv("ACNTNG_PAYCHECK_MATCH"); raw != "" {
		pattern = raw
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return opts, fmt.Errorf("ACNTNG_PAYCHECK_MATCH is not a regular expression: %w", err)
	}
	opts.PaycheckMatch = re

	return opts, nil
}

func runPlanCLI(ctx context.Context, s *Server, asJSON bool) int {
	p, err := s.plan(ctx, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building plan: %v\n", err)
		return 1
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(p); err != nil {
			fmt.Fprintf(os.Stderr, "error encoding json: %v\n", err)
			return 1
		}
		return 0
	}

	if err := printPlan(os.Stdout, p); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func runCLI(ctx context.Context, client Fetcher, overrides map[string]float64, monthStr string, asJSON bool) int {
	now := time.Now()
	at := now
	if monthStr != "" {
		parsed, err := time.Parse("2006-01", monthStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid month %q (expected YYYY-MM): %v\n", monthStr, err)
			return 1
		}
		at = parsed
	}

	budgetRep, err := BuildBudgetReport(ctx, client, at)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error fetching budget report: %v\n", err)
		return 1
	}

	opts := Options{IncludeCredit: true, Overrides: overrides}
	loanRep, err := BuildReport(ctx, client, at, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error fetching loan report: %v\n", err)
		return 1
	}

	if asJSON {
		out := map[string]any{
			"budget": budgetRep,
			"loans":  loanRep,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "error encoding json: %v\n", err)
			return 1
		}
		return 0
	}

	printCLIReport(budgetRep, loanRep)
	return 0
}

func printCLIReport(budget *BudgetReport, loans *Report) {
	fmt.Printf("\n=== acntng report: %s ===\n\n", budget.Month)

	bt := budget.Totals
	fmt.Printf("BUDGET TOTALS\n")
	fmt.Printf("  Income Budgeted:   %s  (Actual: %s)\n", money(bt.IncomeBudgeted), money(bt.IncomeActual))
	fmt.Printf("  Debt Budgeted:     %s  (Spent:  %s)\n", money(bt.DebtBudgeted), money(bt.DebtSpent))
	fmt.Printf("  Living Budgeted:   %s  (Spent:  %s)\n", money(bt.LivingBudgeted), money(bt.LivingSpent))
	if bt.UncategorizedSpent > 0 {
		fmt.Printf("  Uncategorized:     —  (Spent:  %s across %d txs)\n", money(bt.UncategorizedSpent), bt.UncategorizedCount)
	}
	fmt.Printf("  Total Outflow:     %s  (Spent:  %s)\n", money(bt.OutflowBudgeted), money(bt.OutflowSpent))
	fmt.Printf("  Planned Surplus:   %s\n", money(bt.PlannedSurplus))
	fmt.Printf("  Actual Surplus:    %s\n\n", money(bt.ActualSurplus))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	if len(budget.Income) > 0 {
		fmt.Println("INCOME")
		fmt.Fprintln(w, "  Category\tBudgeted\tActual\tRemaining\t% Received")
		for _, l := range budget.Income {
			pctStr := "—"
			if l.PctUsed != nil {
				pctStr = fmt.Sprintf("%.0f%%", *l.PctUsed)
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", l.Name, money(l.Budgeted), money(l.Spent), money(l.Remaining), pctStr)
		}
		_ = w.Flush()
		fmt.Println()
	}

	if len(budget.Debt) > 0 {
		fmt.Println("DEBT SERVICE")
		fmt.Fprintln(w, "  Category\tBudgeted\tSpent\tRemaining\t% Used")
		for _, l := range budget.Debt {
			pctStr := "—"
			if l.PctUsed != nil {
				pctStr = fmt.Sprintf("%.0f%%", *l.PctUsed)
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", l.Name, money(l.Budgeted), money(l.Spent), money(l.Remaining), pctStr)
		}
		_ = w.Flush()
		fmt.Println()
	}

	if len(budget.Living) > 0 {
		fmt.Println("LIVING EXPENSES")
		fmt.Fprintln(w, "  Category\tBudgeted\tSpent\tRemaining\t% Used")
		for _, l := range budget.Living {
			pctStr := "—"
			if l.PctUsed != nil {
				pctStr = fmt.Sprintf("%.0f%%", *l.PctUsed)
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", l.Name, money(l.Budgeted), money(l.Spent), money(l.Remaining), pctStr)
		}
		_ = w.Flush()
		fmt.Println()
	}

	lt := loans.Totals
	fmt.Println("DEBT & ACCOUNTS")
	if lt.Balance > 0 || lt.LiquidCash > 0 || lt.CreditUtilization != nil {
		fmt.Printf("  Total Debt: %s | Liquid Cash: %s | Net Debt: %s\n", money(lt.Balance), money(lt.LiquidCash), money(lt.NetDebt))
		if lt.CreditUtilization != nil {
			fmt.Printf("  Revolving Credit Utilization: %.0f%% (%s / %s)\n", *lt.CreditUtilization, money(lt.TotalCreditBalance), money(lt.TotalCreditLimit))
		}
		fmt.Println()
	}

	fmt.Fprintln(w, "  Account\tBalance\tLimit\tUtil\tMonthly\tSource")
	for _, l := range loans.Loans {
		name := l.Name
		if l.DisplayName != "" {
			name = l.DisplayName
		}
		limStr := "—"
		if l.CreditLimit != nil {
			limStr = money(*l.CreditLimit)
		}
		utilStr := "—"
		if l.Utilization != nil {
			utilStr = fmt.Sprintf("%.0f%%", *l.Utilization)
		}
		moStr := "—"
		if l.MonthlyPayment != nil {
			moStr = money(*l.MonthlyPayment)
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n", name, money(l.Balance), limStr, utilStr, moStr, l.PaymentSource)
	}
	fmt.Fprintf(w, "  TOTAL (%d accounts)\t%s\t%s\t%s\t%s\t\n", lt.Count, money(lt.Balance), money(lt.TotalCreditLimit), utilStrOrEmpty(lt.CreditUtilization), money(lt.MonthlyPayment))
	_ = w.Flush()
	fmt.Println()

	if len(budget.Notes) > 0 || len(loans.Notes) > 0 {
		fmt.Println("NOTES")
		for _, n := range budget.Notes {
			fmt.Printf("  • %s\n", n)
		}
		for _, n := range loans.Notes {
			fmt.Printf("  • %s\n", n)
		}
		fmt.Println()
	}
}

func utilStrOrEmpty(p *float64) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%.0f%%", *p)
}
