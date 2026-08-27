# acntng

Why have vowels when you're in debt

A small JSON service that reports the loans in a [Lunch Money](https://lunchmoney.app) account with their balances and monthly payments. No web UI — it returns JSON and nothing else. It runs on `mist` behind Caddy, which supplies the authentication.

## Endpoints

| Path | Description |
| --- | --- |
| `GET /` | The loan report |
| `GET /loans` | The same report |
| `GET /healthz` | Liveness check |
| `GET /metrics` | Prometheus metrics |

### Query parameters

| Parameter | Default | Description |
| --- | --- | --- |
| `include_credit` | `false` | Fold in credit cards and other revolving credit. |
| `include_liabilities` | `false` | Fold in assets typed `other liability`. |

## Example

```console
$ curl -s https://newyork.welch.io/acntng/ | jq
```

```json
{
  "generated_at": "2026-08-27T01:30:00Z",
  "currency": "USD",
  "totals": {
    "count": 2,
    "balance": 262000,
    "monthly_payment": 1810,
    "loans_missing_payment": 0
  },
  "loans": [
    {
      "id": "plaid:10",
      "source": "plaid",
      "account_id": 10,
      "name": "Mortgage",
      "institution_name": "Chase",
      "type": "loan",
      "subtype": "mortgage",
      "status": "active",
      "currency": "USD",
      "balance": 250000,
      "balance_raw": "250000.0000",
      "balance_as_of": "2026-08-26T12:00:00Z",
      "monthly_payment": 1500,
      "payment_source": "recurring_account_link",
      "payments": [
        {
          "id": 100,
          "payee": "Chase Home Lending",
          "amount": 1500,
          "currency": "USD",
          "cadence": "monthly",
          "monthly_amount": 1500,
          "matched_by": "recurring_account_link"
        }
      ]
    }
  ]
}
```

## How the numbers are derived

**What counts as a loan.** Manually managed assets whose `type_name` is `loan`, plus Plaid accounts whose `type` is `loan`. Closed and inactive accounts are dropped. Credit cards are *excluded* by default — revolving credit is a different thing from a loan — as are `other liability` assets; both are available via query parameter.

**Balances** are the amount owed, expressed positive, which is how Lunch Money reports them. They are not negated into a net-worth sign convention. Each loan carries both a parsed numeric `balance` and the API's original `balance_raw` string.

**Monthly payments are derived, not reported.** The Lunch Money API has no monthly-payment field, so acntng infers one from recurring expenses and tells you how it did it in `payment_source`:

| `payment_source` | Meaning |
| --- | --- |
| `recurring_account_link` | A recurring expense pointed at the account by ID. Trustworthy. |
| `recurring_payee_match` | A recurring expense's payee name matched the loan's name or institution. A heuristic. |
| `override` | Taken from configuration (see `ACNTNG_PAYMENT_OVERRIDES`). |
| `none` | Nothing matched. `monthly_payment` is `null`. |

The payee heuristic carries most of the weight in practice: a loan payment is usually booked against the *checking account it is paid from* rather than against the loan, so the ID link is empty for most loans. When nothing matches at all, `monthly_payment` is `null` rather than `0` so an unknown can be told apart from a genuine zero, and `totals.loans_missing_payment` counts them.

Cadences are normalized to a monthly figure (`weekly` × 52/12, `every 2 weeks` × 26/12, `twice a month` × 2, `every 3 months` ÷ 3, `yearly` ÷ 12, and so on). A `once` cadence is not a recurring obligation and is skipped.

Totals in a mixed-currency account are a raw sum with no conversion, and say so in `notes`.

## Configuration

| Variable | Required | Description |
| --- | --- | --- |
| `LUNCHMONEY_TOKEN` | yes | Lunch Money API token, from [developers.lunchmoney.app](https://developers.lunchmoney.app/). |
| `PORT` | no | Listen port. Defaults to `8080`. |
| `ACNTNG_PAYMENT_OVERRIDES` | no | JSON object mapping a loan ID to its monthly payment, e.g. `{"asset:12": 450.50}`. For loans whose payment is not modeled as a recurring expense at all. |
| `NAT_ENV` | no | Set to `production` in the container to enable the strict security headers. |

Reports are cached for five minutes; the Lunch Money API is rate limited and a balance moves at most daily.

## Deployment

The container runs on `mist` and is reachable at `https://newyork.welch.io/acntng/`, behind the `caddy-home` authentication portal. See [`icco/caddy-home`](https://github.com/icco/caddy-home) for the route and [`icco.me/mist`](https://github.com/icco/icco.me) for the compose service.

## Development

```console
$ export LUNCHMONEY_TOKEN=...
$ task run
$ curl -s localhost:8080/ | jq
```

`task` runs build, vet and test. See `task --list-all`.
