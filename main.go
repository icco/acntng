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
			Log:       log,
			Client:    lm,
			Overrides: overrides,
			SharedKey: sharedKey,
			Now:       time.Now,
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

// Server holds the request-scoped dependencies for the HTTP handlers.
type Server struct {
	Log       *zap.SugaredLogger
	Client    LoanFetcher
	Overrides map[string]float64
	// SharedKey, when set, is required on report requests.
	SharedKey string
	// Now is injectable so tests can pin the reporting month.
	Now func() time.Time

	cache cache
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
		r.Get("/", s.handleLoans)
		r.Get("/loans", s.handleLoans)
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

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}

	key := fmt.Sprintf("credit=%t&liabilities=%t", opts.IncludeCredit, opts.IncludeLiabilities)
	if rep, ok := s.cache.get(key, now()); ok {
		render.JSON(log, w, http.StatusOK, rep)
		return
	}

	rep, err := BuildReport(r.Context(), s.Client, now(), opts)
	if err != nil {
		log.Errorw("could not build loan report", zap.Error(err))
		render.JSON(log, w, http.StatusBadGateway,
			map[string]string{"error": "could not read from lunch money"})
		return
	}

	s.cache.set(key, rep, now())
	render.JSON(log, w, http.StatusOK, rep)
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
