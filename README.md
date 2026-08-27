# acntng

Why have vowels when you're in debt

Reports the loans in a [Lunch Money](https://lunchmoney.app) account with their balances and monthly payments, as JSON. No web UI. Runs on `mist` behind Caddy, which supplies the auth.

## Endpoints

| Path | Description |
| --- | --- |
| `GET /`, `GET /loans` | The loan report |
| `GET /healthz` | Liveness check |
| `GET /metrics` | Prometheus metrics |

Query params: `include_credit` and `include_liabilities` (both default `false`).

## Example

```console
$ curl -s https://newyork.welch.io/acntng/ | jq
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

## How the numbers are derived

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
$ curl -s localhost:8080/ | jq
```

`task` runs build, vet and test.
