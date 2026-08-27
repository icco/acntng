# acntng

Why have vowels when you're in debt

A [Lunch Money](https://lunchmoney.app) budget dashboard: budget vs actual for a month, plus loan balances and derived monthly payments. Runs on `mist` behind Caddy, which supplies the auth.

Nothing is stored — every figure is read live per request and cached five minutes.

## Endpoints

| Path | |
| --- | --- |
| `GET /` | Dashboard: the month's budget, then every debt. HTML. |
| `GET /budget` | The month's budget. JSON. |
| `GET /loans` | Loan balances and payments. JSON. |
| `GET /healthz` | Liveness |
| `GET /metrics` | Prometheus |

`/` and `/budget` take `month=YYYY-MM`, defaulting to the current month. `/loans` takes `include_credit` and `include_liabilities`, both default `false`; the dashboard always includes credit, since on a debt view the cards are usually the expensive part.

```console
$ curl -s "https://newyork.welch.io/acntng/budget?month=2026-09" | jq '.totals'
```

```json
{
  "income_budgeted": 5000,
  "income_actual": 2500,
  "income_basis": "budgeted",
  "debt_budgeted": 1750,
  "debt_spent": 1500,
  "living_budgeted": 2000,
  "living_spent": 900,
  "outflow_budgeted": 3750,
  "outflow_spent": 2400,
  "planned_surplus": 1250,
  "actual_surplus": 100,
  "debt_share": 0.35,
  "categories_over": 1
}
```

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
| `LUNCHMONEY_TOKEN` | yes | From [developers.lunchmoney.app](https://developers.lunchmoney.app/). |
| `ACNTNG_SHARED_KEY` | in production | Secret Caddy injects as `X-Acntng-Key`. Required on report routes when set; the service refuses to start without it when `NAT_ENV=production`. |
| `ACNTNG_PAYMENT_OVERRIDES` | no | JSON mapping a loan ID to its monthly payment, e.g. `{"plaid:1001": 1500}`. |
| `PORT` | no | Defaults to `8080`. |
| `NAT_ENV` | no | `production` enables strict security headers and requires the shared key. |

## Why a shared key as well as the portal

The portal protects the *public* route, but ~40 siblings on mist's shared `caddy` network reach `acntng:8080` directly — including `hoarder`, which crawls user-submitted URLs through a `chrome` on `0.0.0.0:9222`. The portal's `X-WEBAUTH-USER` is forgeable by anything reaching the port, so Caddy injects a secret instead. `/healthz` and `/metrics` are exempt; neither exposes account data.

## Deployment

`https://newyork.welch.io/acntng/`. Route in [`icco/caddy-home`](https://github.com/icco/caddy-home), compose service in [`icco.me`](https://github.com/icco/icco.me).

## Development

```console
$ export LUNCHMONEY_TOKEN=...
$ task run   # no shared key needed outside production
$ open http://localhost:8080/
```

`task` runs build, vet and test.
