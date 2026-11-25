# acntng

Why have vowels when you're in debt

Reads every paycheck and charge in [Lunch Money](https://lunchmoney.app) and says what to change next month to leave `ACNTNG_SAVINGS_TARGET` over. Runs on `mist` behind Caddy, which supplies the auth.

## Usage

```console
$ export LUNCH_MONEY_KEY=...  # or in .env
$ task run                    # http://localhost:8080/
$ task cli                    # the same plan in the terminal
$ task cli -- -json
$ task cli -- -ai             # let the model choose the changes; one call per run
$ task cli -- -debts          # the budget and debt report
```

| Path | |
| --- | --- |
| `GET /` | Next month's plan. HTML. |
| `GET /plan` | Next month's plan. JSON. |
| `GET /dashboard` | Budget vs actual, and every debt. HTML. |
| `GET /budget` | The month's budget. JSON. |
| `GET /loans` | Loan balances and payments. JSON. |
| `GET /healthz` | Liveness |
| `GET /metrics` | Prometheus |

The plan always covers the month after today and takes no params. `/dashboard` and `/budget` take `month=YYYY-MM`, defaulting to the current month. `/loans` takes `include_credit` and `include_liabilities`, both default `false`.

## The plan

Every figure comes from code in `plan.go`, `bills.go`, `changes.go` and `options.go`; a model, when configured, only chooses which changes matter. It reads the last `ACNTNG_HISTORY_MONTHS` complete months plus the month so far.

- **Usual** is a category's median month over the whole history. **Lately** is its median month from `ACNTNG_SINCE` on, or over the last three months without it. The month in progress counts from the 15th, scaled up to a full month.
- **Income** is each paycheck source's typical month lately. Paychecks are income deposits whose payee matches `ACNTNG_PAYCHECK_MATCH`; other income is not counted on. A source that has stopped paying drops out.
- **Bills** come from debt categories and `ACNTNG_FIXED_CATEGORIES`, payee by payee. A payee that charged in two of the last three months is a bill at its typical recent month. One that has gone quiet for 45 days, or whose loan balance is zero, is listed as no longer due. A payee seen in at most two months is occasional, and each category's occasional costs are spread over the year.
- **One-offs** are left out of lately and listed: a charge over $500 from a payee that never charges within 25% of it again, and a debt payment over $5,000 and three times the payee's others.
- **Spending** is every other category at its lately figure. Fees and interest are one line.

## Choosing the changes

Code lists every lever as an option with its saving worked out: each fee kind, each bill (with `ACNTNG_SINCE`, only those from before it; without it, any but rent, mortgage or medical), each steady monthly charge, each category brought to its usual or its leanest quarter, and the top 20 merchants brought to their usual or halved.

With `GEMINI_API_KEY` set, the model reads the facts (income, bills, twelve months per category, one-offs, the gap to the target) and the options, and picks 3 to 7 by id, most important first, with a reason for each. Code applies them in that order, crediting each only what it actually removes, so two picks on the same money are not counted twice. Titles and figures are the code's; the reasons and the summary are the model's.

- One request through the [LiteLLM](https://docs.litellm.ai/) proxy on `gemini-3.5-flash-lite`, thinking off.
- Kept by the facts it was asked about, so the page does not call the model again until the numbers change. At most `ACNTNG_AI_MAX_CALLS` calls a UTC day, each recorded before it is made; a failure is retried after an hour.
- With `DATABASE_URL`, every call and its picks live in the Postgres table `acntng_rankings`, so they survive a restart and the cap holds across processes. Without it they are held in memory.
- The prompt holds category and merchant figures and payee names. No transactions, accounts or paycheck sources.

Without a key, or when the call fails, the rules below choose instead, biggest saving first, and the page says which chose:

| Kind | When |
| --- | --- |
| `fee` | Interest, foreign transaction, overdraft and returned-payment, service, or other fees charged lately. Saving: all of it. |
| `old_bill` | A home, utility, phone or insurance bill that began before `ACNTNG_SINCE` and still charges. Needs `ACNTNG_SINCE`. |
| `creep` | A category lately at least 10% and $50 over its usual. Target: usual. Names the payees behind it. |
| `cut` | Only while the target is still out of reach: categories with the most room above their leanest quarter of months, held there. A cut to a category that already crept is folded into that change. |

When every change still falls short, the plan says so and how much of the pay the bills take.

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
| `ACNTNG_SAVINGS_TARGET` | no | What next month should leave over, in the base currency. Defaults to `1000`. |
| `ACNTNG_SINCE` | no | `YYYY-MM` your current way of living began, such as a move. Lately is judged from it, and bills from before it are flagged. |
| `ACNTNG_HISTORY_MONTHS` | no | Complete months read, 1 to 36. Defaults to `12`. |
| `GEMINI_API_KEY` | no | This service's LiteLLM virtual key. Lets the model choose the changes. |
| `GOOGLE_GEMINI_BASE_URL` | with the key | The LiteLLM proxy, e.g. `http://litellm:4000`. The service refuses to start with a key and no proxy, so the key never reaches Google directly. |
| `GEMINI_MODEL` | no | Model name as LiteLLM routes it. Defaults to `gemini-3.5-flash-lite`. |
| `ACNTNG_AI_MAX_CALLS` | no | Model calls allowed per day. Defaults to `4`. |
| `DATABASE_URL` | no | Postgres DSN where rankings are kept. |
| `ACNTNG_FIXED_CATEGORIES` | no | Comma-separated categories read as bills rather than spending, replacing the default list in `plan.go`. Debt categories always are. |
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
