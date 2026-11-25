# acntng

Why have vowels when you're in debt

Reads paychecks and spending from [Lunch Money](https://lunchmoney.app), works out what a normal month costs, and says what to do differently next month to spend less. Runs on `mist` behind Caddy, which supplies the auth.

## Usage

```console
$ export LUNCH_MONEY_KEY=...  # or in .env
$ task run                    # http://localhost:8080/
$ task cli                    # the same plan in the terminal
$ task cli -- -month 2026-09 -json
$ task cli -- -ai             # also ask the model
$ task cli -- -debts          # the budget and debt report
```

| Path | |
| --- | --- |
| `GET /` | The plan. HTML. |
| `GET /plan` | The plan. JSON. |
| `POST /advice` | Spend one model call on advice for the month, then redirect to `/`. The only route that calls the model. |
| `GET /dashboard` | Budget vs actual, and every debt. HTML. |
| `GET /budget` | The month's budget. JSON. |
| `GET /loans` | Loan balances and payments. JSON. |
| `GET /healthz` | Liveness |
| `GET /metrics` | Prometheus |

`/`, `/plan` and `/advice` take `month=YYYY-MM`, the month to review, defaulting to the last complete month. `/dashboard` and `/budget` take the same param and default to the current month. `/loans` takes `include_credit` and `include_liabilities`, both default `false`.

## The plan

Everything is computed from transactions, in `plan.go` and `suggest.go`. No model is needed for any figure.

- **Paychecks** are income-category deposits whose payee matches `ACNTNG_PAYCHECK_MATCH`. Interest, dividends and refunds are other income, shown but not counted on.
- **Normal** is each category's median over the baseline: the `ACNTNG_BASELINE_MONTHS` months before the review month. A median ignores the one month with a flight in it. Months before the account's history starts are dropped.
- **Fixed** categories are debt service plus `ACNTNG_FIXED_CATEGORIES`. They are reported and never trimmed. Everything else is flexible.
- **Left over** is paychecks less spending, for a normal month and for the review month.
- Categories flagged "exclude from totals" or "exclude from budgets" are skipped, as are pending transactions and uncategorized credits.

Suggestions come from rules, each with a saving measured against the review month:

| Kind | When |
| --- | --- |
| `shortfall` | A normal month spends more than the paychecks. Listed first. |
| `over_normal` | A flexible category ran 10% and $25 over its normal. Target: normal. |
| `trim` | The three flexible categories with the most room above their 25th percentile month. Target: that month. |
| `fees` | Anything in a category with the word fee or fees in its name. Target: zero. |
| `habit` | Four or more purchases at one merchant totalling $60. Not added to the total, since it sits inside a category. |
| `subscriptions` | Charges landing once a month at a steady amount for three months running. No saving is claimed. |
| `uncategorized` | Over 5% of the month has no category. |

`totals.potential_savings` sums the category suggestions; `totals.target_spend` sums the per-category targets. With no month of history before the review month there is no normal, so each target is the month's own spending and only `habit` and `uncategorized` apply.

## AI advice

Optional, and always through the [LiteLLM](https://docs.litellm.ai/) proxy, which attributes spend to this service's own virtual key. With a key set, the Generate button sends the plan's aggregates to the model, which picks up to five of the rule-based suggestions and writes a specific action for each. Titles and savings stay the rules' own, so no dollar figure on the page is a model's guess. Spend is kept low by construction:

- Viewing never calls the model. `GET /` and `GET /plan` read stored advice; only `POST /advice` (the Generate and Regenerate buttons) and `-cli -ai` generate.
- One request each time, with thinking off, on `gemini-3.5-flash-lite`. No agent loop and no tool calls. A real month measured 3,320 tokens in and 670 out.
- At most `ACNTNG_AI_MAX_CALLS` requests per calendar month. Each is recorded before the model is called, so failures and dropped connections still count.
- The prompt holds category totals, merchant names and the rule-based suggestions. No transactions, account names, or paycheck sources.

With `DATABASE_URL` the advice and the call count live in the Postgres table `acntng_advice` and survive a restart. Without it they are held in memory, and a restart resets the budget.

## The budget report

One row per category, split three ways:

- **Income** — Lunch Money's `is_income` flag. Reported as a magnitude, since income arrives as a credit.
- **Debt service** — matched by name; the API has no "this is a debt" flag. Names are hardcoded in `budget.go`.
- **Living** — the rest.

Two kinds of row are dropped, because counting them inflates the totals: **category groups**, which restate their own children, and **categories flagged "exclude from budgets"** — transfers and card payments, which settle spending already counted elsewhere. The count lands in `notes`. Categories with neither a budget nor activity are omitted.

`pct_used` is `null` rather than `0` when nothing is budgeted, so "spent off budget" stays distinct from "budgeted and untouched".

`planned_surplus` needs an income figure, and Lunch Money treats income as observed rather than planned. It measures against budgeted income when one is set and income received otherwise; `totals.income_basis` says which. With neither it is just the negated outflow, and `notes` says so.

## The loan report

**Loans** are assets typed `loan` plus Plaid accounts typed `loan`; closed and inactive are dropped. Credit cards and `other liability` assets opt in by query param.

**Balances** are the amount owed, positive, as Lunch Money reports them — not negated into a net-worth convention.

**Monthly payments are derived**, since the API has no such field. `payment_source` says how each was found:

| | |
| --- | --- |
| `recurring_account_link` | A recurring expense linked to the account by ID. Reliable. |
| `recurring_payee_match` | Its payee matched the loan name. A heuristic. |
| `override` | From `ACNTNG_PAYMENT_OVERRIDES`. |
| `none` | Nothing matched; `monthly_payment` is `null`, not `0`. |

The payee heuristic does most of the work, because a payment is usually booked against the checking account it's paid *from*. `totals.loans_missing_payment` counts the gaps.

Cadences normalize to monthly (`weekly` × 52/12, `every 2 weeks` × 26/12, `every 3 months` ÷ 3); `once` is skipped. One expense credits one loan — its most specific name match — since crediting every match would double the total. Ties land in `notes`, as do mixed-currency totals, which are an unconverted raw sum.

## Configuration

| Variable | Required | |
| --- | --- | --- |
| `LUNCHMONEY_TOKEN`, `LUNCH_MONEY_KEY`, or `LUNCHMONEY_API_TOKEN` | yes | From [developers.lunchmoney.app](https://developers.lunchmoney.app/). |
| `ACNTNG_SHARED_KEY` | in production | Secret Caddy injects as `X-Acntng-Key`. Required on report routes when set; the service refuses to start without it when `NAT_ENV=production`. |
| `ACNTNG_PAYCHECK_MATCH` | no | Regular expression matched against an income payee. Defaults to common payroll descriptors (`payroll`, `gusto`, `adp`, `direct deposit`, ...). |
| `ACNTNG_BASELINE_MONTHS` | no | Months that define normal, 1 to 24. Defaults to `6`. |
| `ACNTNG_FIXED_CATEGORIES` | no | Comma-separated category names never trimmed, replacing the default list in `plan.go`. Debt categories are always fixed. |
| `GEMINI_API_KEY` | no | This service's LiteLLM virtual key. Turns on AI advice. |
| `GOOGLE_GEMINI_BASE_URL` | with the key | The LiteLLM proxy, e.g. `http://litellm:4000`. The service refuses to start with a key and no proxy, so the key is never sent to Google directly. |
| `GEMINI_MODEL` | no | Model name as LiteLLM routes it. Defaults to `gemini-3.5-flash-lite`. |
| `ACNTNG_AI_MAX_CALLS` | no | Model calls allowed per calendar month. Defaults to `4`; `0` serves stored advice only. |
| `DATABASE_URL` | no | Postgres DSN for stored advice. |
| `ACNTNG_PAYMENT_OVERRIDES` | no | JSON mapping a loan ID to its monthly payment, e.g. `{"plaid:1001": 1500}`. |
| `PORT` | no | Defaults to `8080`. |
| `NAT_ENV` | no | `production` enables strict security headers and requires the shared key. |

## Why a shared key as well as the portal

The portal protects the *public* route, but ~40 siblings on mist's shared `caddy` network reach `acntng:8080` directly — including `hoarder`, which crawls user-submitted URLs through a `chrome` on `0.0.0.0:9222`. The portal's `X-WEBAUTH-USER` is forgeable by anything reaching the port, so Caddy injects a secret instead. `/healthz` and `/metrics` are exempt; neither exposes account data.

## Deployment

`https://newyork.welch.io/acntng/`. Route in [`icco/caddy-home`](https://github.com/icco/caddy-home), compose service in [`icco.me`](https://github.com/icco/icco.me).

## Development

```console
$ export LUNCH_MONEY_KEY=...  # or in .env
$ task run   # no shared key needed outside production
$ open http://localhost:8080/
```

`task` runs build, vet and test. Run `task check` to verify formatting, vet, and race testing.

The Postgres store test starts an embedded Postgres, downloading it on first run. `go test -short ./...` skips it, and `TEST_DATABASE_URL` points it at a database of your own.
