# AGENTS.md

Guidance for coding agents working on acntng.

## Project Overview

Monthly frugality plan from Lunch Money paychecks and spending, as a web page, JSON API and CLI, written in Go (`go.icco.me/acntng`).

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
- `plan.go` — Paychecks, normal spend and the review month, from transactions.
- `suggest.go` — Rule-based suggestions and per-category targets. Every figure comes from here, never from a model.
- `advice.go` — The optional Gemini call, made through the LiteLLM proxy, and the `Coach` that caps and caches it.
- `store.go` — Advice storage: Postgres via GORM, or memory.
- `budget.go`, `loans.go` — The older budget and debt reports behind `/dashboard`.
- `templates/` — HTML templates; `style.html` is shared.

## Conventions

- Follow icco Go conventions: `go.icco.me/gutil` for logging and helpers.
- PR titles and commits must follow Conventional Commits with lowercase subjects.
- Ensure `task lint` and `task test` pass before committing.
- Keep model spend low: one request per explicit `POST /advice`, thinking off, reserved against `ACNTNG_AI_MAX_CALLS` before the call. GET routes never call the model.
- The model picks and words rule-based suggestions by ID. Titles and dollar figures always come from `suggest.go`.
- Test fixtures use invented payees and amounts, never real Lunch Money data.
