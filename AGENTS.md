# AGENTS.md

Guidance for coding agents working on acntng.

## Project Overview

Says what to change next month, from every paycheck and charge in Lunch Money, as a web page, JSON API and CLI, written in Go (`go.icco.me/acntng`).

## Commands (Taskfile)

Run via `task <name>`:
- `task build` — Build `acntng` binary
- `task test` — Run unit tests with race detection (`go test -race ./...`)
- `task lint` — Run `golangci-lint run`
- `task vet` — Run `go vet ./...`
- `task fmt` — Format Go code (`gofmt -w .`)
- `task run` — Run local server on port 8080
- `task cli -- <args>` — Run report directly in CLI mode

## Architecture & Layout

- `main.go` — Server setup, environment, CLI flag dispatch, and HTTP routes.
- `plan.go` — Reads the history, sorts it into paychecks and charges, and totals the plan.
- `bills.go` — Bills, debts paid off or stopped, occasional costs and one-offs, payee by payee.
- `changes.go` — Spending caps and the rule-chosen changes, the fallback when no model ranks.
- `options.go` — Every lever with its saving, and applying the picked ones.
- `rank.go` — The model call through LiteLLM that picks options, and its daily cache.
- `store.go` — Every model call and its picks: Postgres via GORM, or memory.
- `planview.go` — The plan's handlers, verdict and terminal output.
- `budget.go`, `loans.go` — The budget and debt reports behind `/dashboard`.
- `templates/` — HTML templates; `style.html` is shared.

## Conventions

- Follow icco Go conventions: `go.icco.me/gutil` for logging and helpers.
- PR titles and commits must follow Conventional Commits with lowercase subjects.
- Ensure `task lint` and `task test` pass before committing.
- The plan page has no month picker: it always plans the month after today from all history.
- The model only picks option ids and writes reasons. Titles and dollar figures always come from code, and GET routes call it at most once a day per set of facts.
- Test fixtures use invented payees and amounts, never real Lunch Money data.
