# acntng

Why have vowels when you're in debt

Views and manages a [Lunch Money](https://lunchmoney.app) budget: a monthly budget-vs-actual dashboard, plus loan balances and derived monthly payments. HTML at `/`, JSON at `/budget` and `/loans`. Runs on `mist` behind Caddy, which supplies the auth.

Nothing is stored. Every figure is read live from the Lunch Money API on request and cached five minutes.

## Endpoints

| Path | Description |
| --- | --- |
| `GET /` | Dashboard: budget vs actual for the month, then every debt. HTML. |
| `GET /budget` | The budget report for one month. JSON. |
| `GET /loans` | The loan report. JSON. |
| `GET /healthz` | Liveness check |
| `GET /metrics` | Prometheus metrics |

`/` and `/budget` take `month=YYYY-MM`, defaulting to the current month. The dashboard links to the neighbouring months. `/loans` takes `include_credit` and `include_liabilities` (both default `false`); the dashboard always includes credit, because on a debt view the cards are usually the expensive part.

## Example

```console
$ curl -s "https://newyork.welch.io/acntng/budget?month=2026-09" | jq
```

```json
{
  "generated_at": "2026-09-04T02:00:01Z",
  "month": "2026-09",
  "currency": "USD",
  "totals": {
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
  },
  "income": [{ "category_id": 1, "name": "Income", "is_income": true, "is_debt": false, "budgeted": 5000, "spent": 2500, "remaining": 2500, "pct_used": 50, "transactions": 1 }],
  "debt": [{ "category_id": 2, "name": "Mortgage", "is_income": false, "is_debt": true, "budgeted": 1750, "spent": 1500, "remaining": 250, "pct_used": 85.71, "transactions": 1 }],
  "living": [{ "category_id": 3, "name": "Groceries", "is_income": false, "is_debt": false, "budgeted": 2000, "spent": 900, "remaining": 1100, "pct_used": 45, "transactions": 12 }]
}
```

```console
$ curl -s https://newyork.welch.io/acntng/loans | jq
```

```json
{
  "generated_at": "2026-08-27T02:00:01Z",
  "currency": "USD",
  "totals": {
    "count": 2,
    "balance": 31000,
    "monthly_payment": 1750,
    "loans_missing_payment": 0
  },
  "loans": [
    {
      "id": "plaid:1001",
      "source": "plaid",
      "account_id": 1001,
      "name": "MORTGAGE ...0000",
      "institution_name": "Example Bank",
      "type": "loan",
      "subtype": "mortgage",
      "status": "active",
      "currency": "USD",
      "balance": 25000,
      "balance_raw": "25000.0000",
      "balance_as_of": "2026-08-27T01:28:07Z",
      "monthly_payment": 1500,
      "payment_source": "override"
    },
    {
      "id": "asset:2002",
      "source": "asset",
      "account_id": 2002,
      "name": "Personal Loan",
      "institution_name": "Example Credit Union",
      "type": "loan",
      "subtype": "consumer",
      "status": "active",
      "currency": "USD",
      "balance": 6000,
      "balance_raw": "6000.0000",
      "balance_as_of": "2026-08-27T01:35:58Z",
      "monthly_payment": 250,
      "payment_source": "recurring_payee_match"
    }
  ]
}
```

## How the budget is derived

One row per category for the requested month, split three ways:

- **Income** — categories Lunch Money flags `is_income`. Reported as a magnitude, since income arrives as a credit.
- **Debt service** — categories named in `ACNTNG_DEBT_CATEGORIES`, defaulting to mortgage, student loans, auto loan, personal loans, buy now pay later, loan payments, credit card payments. Lunch Money has no "this category is a debt" flag, so the split is by name.
- **Living** — everything else.

Two kinds of row are dropped, because counting them inflates every total:

- **Category groups**, which restate their own children.
- **Categories flagged "exclude from budgets"** — transfers and credit-card payments. That movement is real cash, but it is settlement of spending already counted in another category, so including it double-counts every purchase. The count lands in `notes`.

Categories with no budget *and* no activity are omitted as noise.

`pct_used` is `null` rather than `0` when nothing is budgeted, so "spent with no budget set" stays distinguishable from "budgeted and untouched".

**The surplus needs an income figure**, and Lunch Money treats income as observed rather than planned, so many accounts budget none. `planned_surplus` is measured against the budgeted income when one exists and against income received so far otherwise — `totals.income_basis` says which, and a note explains it. With neither, the figure is only the negated outflow and says so in `notes`; budget an income category to fix that.

## How the loan numbers are derived

**Loans** are assets typed `loan` plus Plaid accounts typed `loan`; closed and inactive are dropped. Credit cards are excluded by default (revolving credit isn't a loan), as are `other liability` assets. Both opt in by query param.

**Balances** are the amount owed, positive, as Lunch Money reports them — not negated into a net-worth convention. Each loan carries a parsed `balance` and the original `balance_raw`.

**Monthly payments are derived** — the API has no such field. `payment_source` says how each was found:

| `payment_source` | Meaning |
| --- | --- |
| `recurring_account_link` | A recurring expense linked to the account by ID. Reliable. |
| `recurring_payee_match` | A recurring expense's payee matched the loan name. A heuristic. |
| `override` | From `ACNTNG_PAYMENT_OVERRIDES`. |
| `none` | Nothing matched; `monthly_payment` is `null`. |

The payee heuristic does most of the work, since a payment is usually booked against the checking account it's paid *from*. When nothing matches, `monthly_payment` is `null` rather than `0`, and `totals.loans_missing_payment` counts them.

Cadences normalize to monthly (`weekly` × 52/12, `every 2 weeks` × 26/12, `every 3 months` ÷ 3, …); `once` is skipped. One expense credits one loan, its most specific name match — crediting every match would double the total. Ties land in `notes`, as do mixed-currency totals, which are an unconverted raw sum.

## Configuration

| Variable | Required | Description |
| --- | --- | --- |
| `LUNCHMONEY_TOKEN` | yes | From [developers.lunchmoney.app](https://developers.lunchmoney.app/). |
| `ACNTNG_SHARED_KEY` | in production | Secret Caddy injects as `X-Acntng-Key`. Required on the report routes when set; the service refuses to start without it when `NAT_ENV=production`. |
| `ACNTNG_PAYMENT_OVERRIDES` | no | JSON mapping a loan ID to its monthly payment, e.g. `{"plaid:1001": 1500}`. |
| `ACNTNG_DEBT_CATEGORIES` | no | Comma-separated category names to count as debt service, replacing the defaults. Case and surrounding space are ignored. |
| `PORT` | no | Defaults to `8080`. |
| `NAT_ENV` | no | `production` enables strict security headers and requires the shared key. |

Reports are cached five minutes; the API is rate limited.

## Why a shared key as well as the portal

The portal protects the *public* route, but ~40 siblings on mist's shared `caddy` network reach `acntng:8080` directly — including `hoarder`, which crawls user-submitted URLs through a `chrome` on `0.0.0.0:9222`. The portal's `X-WEBAUTH-USER` is forgeable by anything reaching the port, so Caddy injects a secret instead. `/healthz` and `/metrics` are exempt; neither exposes account data.

## Deployment

Runs on `mist` at `https://newyork.welch.io/acntng/`. Route lives in [`icco/caddy-home`](https://github.com/icco/caddy-home); compose service in [`icco.me`](https://github.com/icco/icco.me).

## Development

```console
$ export LUNCHMONEY_TOKEN=...
$ task run   # no shared key needed outside production
$ open http://localhost:8080/
$ curl -s "localhost:8080/budget?month=2026-09" | jq
```

`task` runs build, vet and test.
