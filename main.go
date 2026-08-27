package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/icco/gutil/logging"
	"github.com/icco/gutil/render"
	"github.com/icco/lunchmoney"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/unrolled/secure"
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

func run() int {
	log := logging.Must(logging.NewLogger(serverName))
	defer logging.Sync(log)

	token := os.Getenv("LUNCHMONEY_TOKEN")
	if token == "" {
		log.Errorw("LUNCHMONEY_TOKEN is required")
		return 1
	}

	lm, err := lunchmoney.NewClient(token)
	if err != nil {
		log.Errorw("could not create lunchmoney client", zap.Error(err))
		return 1
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

	overrides, err := parseOverrides(os.Getenv("ACNTNG_PAYMENT_OVERRIDES"))
	if err != nil {
		log.Errorw("could not parse ACNTNG_PAYMENT_OVERRIDES", zap.Error(err))
		return 1
	}
	if len(overrides) > 0 {
		log.Infow("loaded payment overrides", "count", len(overrides))
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
			Log:            log,
			Client:         lm,
			Overrides:      overrides,
			DebtCategories: debtCategoriesFromEnv(),
			SharedKey:      sharedKey,
			Now:            time.Now,
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
}

// Server holds the request-scoped dependencies for the HTTP handlers.
type Server struct {
	Log       *zap.SugaredLogger
	Client    Fetcher
	Overrides map[string]float64
	// DebtCategories names the budget categories that count as debt service.
	DebtCategories map[string]bool
	// SharedKey, when set, is required on report requests.
	SharedKey string
	// Now is injectable so tests can pin the reporting month.
	Now func() time.Time

	loanCache   cache[*Report]
	budgetCache cache[*BudgetReport]
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

	rep, err := BuildBudgetReport(ctx, s.Client, at, s.DebtCategories)
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
	r.Use(otelhttp.NewMiddleware(serverName))

	r.Handle("/metrics", metrics)

	r.Group(func(r chi.Router) {
		r.Use(requireSharedKey(s.SharedKey))
		r.Get("/", s.handleDashboard)
		r.Get("/loans", s.handleLoans)
		r.Get("/budget", s.handleBudget)
	})

	return r
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
