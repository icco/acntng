package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/icco/gutil/logging"
	"github.com/icco/gutil/render"
	"go.uber.org/zap"
)

// tightThreshold is the percent-used mark at which a category turns amber.
const tightThreshold = 85

//go:embed templates/*.html
var templateFS embed.FS

// viewTemplates is parsed once at init: a template parse error is a build
// mistake, not a runtime condition, so failing here fails the binary.
var viewTemplates = template.Must(
	template.New("").Funcs(viewFuncs()).ParseFS(templateFS, "templates/*.html"))

// dashboard is the view model. It carries both reports plus whatever went
// wrong fetching either, so one upstream failure still renders the other half.
type dashboard struct {
	GeneratedAt time.Time
	Month       string
	Budget      *BudgetReport
	Loans       *Report
	BudgetErr   string
	LoansErr    string
}

// HasAnything reports whether there is a report to render. Neither means the
// page is empty and the status should say so.
func (d *dashboard) HasAnything() bool {
	return d.Budget != nil || d.Loans != nil
}

func viewFuncs() template.FuncMap {
	return template.FuncMap{
		// money formats with thousands separators and no cents, which is the
		// resolution budget decisions are actually made at.
		"money": money,
		"pct": func(p *float64) string {
			if p == nil {
				return "—"
			}
			return fmt.Sprintf("%.0f%%", *p)
		},
		"share": func(p *float64) string {
			if p == nil {
				return "—"
			}
			return fmt.Sprintf("%.0f%%", *p*100)
		},
		// bar clamps to 100 so an overspent category fills its track rather
		// than overflowing the container.
		"bar": func(p *float64) string {
			if p == nil {
				return "0"
			}
			return strconv.FormatFloat(math.Min(math.Max(*p, 0), 100), 'f', 1, 64)
		},
		"payment": func(p *float64) string {
			if p == nil {
				return "—"
			}
			return money(*p)
		},
		"monthName": func(s string) string {
			t, err := time.Parse("2006-01", s)
			if err != nil {
				return s
			}
			return t.Format("January 2006")
		},
		"stamp": func(t time.Time) string {
			return t.Format("2 Jan 2006 15:04 MST")
		},
		"sub": func(a, b float64) float64 { return a - b },
		// tight marks a category close enough to its limit to be worth a
		// second colour before it actually goes over.
		"tight": func(p *float64) bool { return p != nil && *p >= tightThreshold && *p <= 100 },
		// shortDate renders an RFC3339 stamp as a bare date, or passes through
		// whatever it was given if it is not a timestamp.
		"shortDate": func(s string) string {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return s
			}
			return t.Format("2 Jan")
		},
	}
}

// money renders a float as a whole-dollar figure with thousands separators.
func money(f float64) string {
	whole := strconv.FormatInt(int64(math.Round(math.Abs(f))), 10)

	var out []byte
	for i := range len(whole) {
		if i > 0 && (len(whole)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, whole[i])
	}

	if f < 0 {
		return "−" + string(out)
	}
	return string(out)
}

// handleDashboard renders the HTML view. Both reports are optional: a failure
// in one is reported inline rather than blanking the page.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	log := logging.FromContext(r.Context())

	now := s.clock()

	at, err := monthFromRequest(r, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	d := &dashboard{GeneratedAt: now.UTC()}

	if rep, err := s.budgetReport(r.Context(), at, now); err != nil {
		log.Errorw("could not build budget report", zap.Error(err))
		d.BudgetErr = "could not read the budget from Lunch Money"
	} else {
		d.Budget = rep
		d.Month = rep.Month
	}

	// Credit cards are revolving debt, but on a debt dashboard leaving them
	// out hides the most expensive balances in the account.
	opts := Options{IncludeCredit: true, Overrides: s.Overrides}
	if rep, err := s.loanReport(r.Context(), opts, now); err != nil {
		log.Errorw("could not build loan report", zap.Error(err))
		d.LoansErr = "could not read loans from Lunch Money"
	} else {
		d.Loans = rep
	}

	// Render to memory first: a template error halfway through a streamed
	// response leaves a truncated page behind a 200 already on the wire.
	body, err := renderTemplate("dashboard.html", d)
	if err != nil {
		log.Errorw("could not render dashboard", zap.Error(err))
		http.Error(w, "could not render the dashboard", http.StatusInternalServerError)
		return
	}

	status := http.StatusOK
	if !d.HasAnything() {
		status = http.StatusBadGateway
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Warnw("could not write dashboard response", zap.Error(err))
	}
}

// renderTemplate executes a named template into a byte slice.
func renderTemplate(name string, data any) ([]byte, error) {
	var b bytes.Buffer
	if err := viewTemplates.ExecuteTemplate(&b, name, data); err != nil {
		return nil, fmt.Errorf("execute %s: %w", name, err)
	}
	return b.Bytes(), nil
}

// handleBudget returns the budget report as JSON.
func (s *Server) handleBudget(w http.ResponseWriter, r *http.Request) {
	log := logging.FromContext(r.Context())

	now := s.clock()

	at, err := monthFromRequest(r, now)
	if err != nil {
		render.JSON(log, w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	rep, err := s.budgetReport(r.Context(), at, now)
	if err != nil {
		log.Errorw("could not build budget report", zap.Error(err))
		render.JSON(log, w, http.StatusBadGateway,
			map[string]string{"error": "could not read from lunch money"})
		return
	}

	render.JSON(log, w, http.StatusOK, rep)
}
