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
    "balance": 411156.04,
    "monthly_payment": 1500,
    "loans_missing_payment": 1
  },
  "loans": [
    {
      "id": "plaid:465191",
      "source": "plaid",
      "account_id": 465191,
      "name": "MORTGAGE ...6776",
      "institution_name": "Wells Fargo",
      "type": "loan",
      "subtype": "mortgage",
      "status": "active",
      "currency": "USD",
      "balance": 375796.38,
      "balance_raw": "375796.3800",
      "balance_as_of": "2026-08-27T01:28:07Z",
      "monthly_payment": 1500,
      "payment_source": "override"
    }
  ]
}
```

## How the numbers are derived

**Loans** are assets with `type_name: loan` plus Plaid accounts with `type: loan`. Closed and inactive accounts are dropped. Credit cards are excluded by default — revolving credit isn't a loan — as are `other liability` assets; both opt in via query param.

**Balances** are the amount owed, positive, as Lunch Money reports them. Not negated into a net-worth convention. Each loan carries a parsed `balance` and the API's original `balance_raw`.

**Monthly payments are derived, not reported** — the API has no such field. `payment_source` says how each was found:

| `payment_source` | Meaning |
| --- | --- |
| `recurring_account_link` | A recurring expense linked to the account by ID. Reliable. |
| `recurring_payee_match` | A recurring expense's payee matched the loan name. A heuristic. |
| `override` | From `ACNTNG_PAYMENT_OVERRIDES`. |
| `none` | Nothing matched; `monthly_payment` is `null`. |

The payee heuristic carries most of the weight: a loan payment is usually booked against the checking account it's paid *from*, not the loan. When nothing matches, `monthly_payment` is `null` rather than `0` so an unknown is distinguishable from a real zero, and `totals.loans_missing_payment` counts them.

Cadences normalize to a monthly figure (`weekly` × 52/12, `every 2 weeks` × 26/12, `every 3 months` ÷ 3, and so on). `once` is skipped. One expense is credited to a single loan — its most specific name match — since crediting every match would double the total; an exact tie is reported in `notes` instead.

Mixed-currency totals are a raw sum with no conversion, and say so in `notes`.

## Configuration

| Variable | Required | Description |
| --- | --- | --- |
| `LUNCHMONEY_TOKEN` | yes | From [developers.lunchmoney.app](https://developers.lunchmoney.app/). |
| `ACNTNG_SHARED_KEY` | in production | Secret Caddy injects as `X-Acntng-Key`. Required on the report routes when set; the service refuses to start without it when `NAT_ENV=production`. |
| `ACNTNG_PAYMENT_OVERRIDES` | no | JSON mapping a loan ID to its monthly payment, e.g. `{"plaid:465191": 1500}`. |
| `PORT` | no | Defaults to `8080`. |
| `NAT_ENV` | no | `production` enables strict security headers and requires the shared key. |

Reports are cached five minutes; the API is rate limited.

## Why a shared key as well as the portal

The portal protects the *public* route, but the container also sits on mist's shared `caddy` network with ~40 siblings that can reach `acntng:8080` by name and skip it — including `hoarder`, which crawls user-submitted URLs through a `chrome` listening on `0.0.0.0:9222`. The portal's injected `X-WEBAUTH-USER` is forgeable by anything that reaches the port, so Caddy injects a secret instead. `/healthz` and `/metrics` are exempt; neither exposes account data.

## Deployment

Runs on `mist` at `https://newyork.welch.io/acntng/`. Route lives in [`icco/caddy-home`](https://github.com/icco/caddy-home); compose service in [`icco.me`](https://github.com/icco/icco.me).

## Development

```console
$ export LUNCHMONEY_TOKEN=...
$ task run   # no shared key needed outside production
$ curl -s localhost:8080/ | jq
```

`task` runs build, vet and test.
