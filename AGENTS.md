# AGENTS.md

Guidance for coding agents working on acntng.

## Project Overview

Personal double-entry accounting web dashboard and CLI reporting tool written in Go (`github.com/icco/acntng`).

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

- `main.go` — Server setup, CLI flag dispatch, and HTTP routes.
- Domain models and ledger handling in root package.
- `templates/` — HTML dashboard templates.

## Conventions

- Follow icco Go conventions: `github.com/icco/gutil` for logging and helpers.
- PR titles and commits must follow Conventional Commits with lowercase subjects.
- Ensure `task lint` and `task test` pass before committing.
