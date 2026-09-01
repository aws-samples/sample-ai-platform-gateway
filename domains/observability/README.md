# Domain: Observability & FinOps (Costs & Usage tab)

Captures, stores, and (in the future) presents usage, cost, latency, and cache. It produces the differentiating data: cost per customer/app.

## Structure (all owned by the domain)
```
observability/
├── build.sh              # compiles the Go Lambda (arm64)
├── src/                  # Go code
│   ├── go.mod
│   └── cmd/
│       └── usage-writer/ # consumes SQS → writes Cost_Store
├── dist/                 # .zip artifact (bootstrap)
└── envs/poc/             # Terraform (this domain's ISOLATED state)
```

## AWS resources (poc)
- Lambdas (Go, `provided.al2023`, arm64): `aiplat-poc-obs-usage-writer` (ingestion), `aiplat-poc-obs-usage-api` (read/aggregation)
- SQS: `aiplat-poc-obs-usage` (+ DLQ `aiplat-poc-obs-usage-dlq`, maxReceiveCount=5)
- DynamoDB: `aiplat-poc-obs-cost-store` (pk=`TENANT#`, sk=`TS#`, GSI `gsi1` by app)
- EventBridge bus: `aiplat-poc-obs-finops`
- Usage API (HTTP API): **`https://<usage-api-id>.execute-api.us-west-2.amazonaws.com`**

## Cost-correlation layer + ROI ledger (the differentiator)
`GET /usage/summary?tenant=<t>&from=<iso>&to=<iso>&bucket=day|hour` (header `x-admin-token`) returns, from the real Cost_Store data, the **unified across-LLMs** view per tenant:
- `totals` — cost, **saved_usd**, **gross_usd**, **saved_pct** (ROI), requests, tokens, cache hits, average latency
- `by_provider`, `by_model`, `by_app`, `by_feature` — breakdown ordered by cost
- `savings_by_reason` — savings by reason (`auto_cheapest`, `fallback`, `cache`)
- `series` — cost/requests per day or hour

The Usage_Record carries `feature`, `saved_usd`, and `savings_reason` (produced by the gateway): savings are measured per request (cost of the requested model − cost of the model actually used; plus cost avoided by the cache) and aggregated here as an auditable ledger.

```bash
curl -s "https://<usage-api-id>.execute-api.us-west-2.amazonaws.com/usage/summary?tenant=acme" \
  -H "x-admin-token: <admin-token>"
```

## Contracts (EDA boundaries)
- **Consumes (Core):** Usage_Record via SQS (asynchronous). Tolerates spikes and reprocesses failures via the DLQ.
- **Produces (→ Governance/Ledger):** Cost_Events and cost aggregates per tenant/app (input to the ROI ledger).

## Build & Deploy
```bash
./build.sh                                   # produces dist/usage-writer.zip
cd envs/poc
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform init
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform apply
```

## Query the Cost_Store
```bash
aws dynamodb scan --table-name aiplat-poc-obs-cost-store --region us-west-2 --max-items 5
```

> Isolation: if this domain goes down, Core keeps serving — events sit in the queue/DLQ and are reprocessed.
