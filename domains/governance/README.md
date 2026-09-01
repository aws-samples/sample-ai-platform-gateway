# Domain: Governance & Config (Settings tab)

Source of truth for dynamic config and provider credentials. It's where the admin changes behavior (auto-cheapest, routing, pricing, cache) **without a redeploy**, and where the console is served.

## Structure (all owned by the domain)
```
governance/
├── build.sh              # compiles the Go Lambda (arm64)
├── src/                  # Go code
│   ├── go.mod
│   └── cmd/
│       └── config-api/   # GET/PUT /admin/config, POST /admin/secrets
├── site/                 # static console (SPA)
│   ├── app/index.html
│   └── env.js.tftpl      # endpoints injected at deploy
├── dist/                 # .zip artifact (bootstrap)
└── envs/poc/             # Terraform (ISOLATED state) + default_config.json (seed)
```

## AWS resources (poc)
- Lambda (Go, `provided.al2023`, arm64): `aiplat-poc-gov-config-api`
- DynamoDB: `aiplat-poc-gov-config` (single item `pk="global"`)
- Admin API (HTTP API): **`https://<admin-api-id>.execute-api.us-west-2.amazonaws.com`**
- Console (private S3 + CloudFront/OAC): **`https://<console-dist>.cloudfront.net`**
- Secrets Manager: provider credentials under `aiplat/gateway/*`

Auth (PoC): header `x-admin-token: <admin-token>`. **Technical debt** — evolve toward Cognito/SSO.

## Contracts (EDA boundaries)
- **Produces (→ Core):** config read at runtime by Core (with fallback). Changes apply to subsequent requests.
- **Produces (→ Observability):** budget thresholds/policies that generate Cost_Events.

## Models domain (folded in here)
The logical **Models & Providers** domain (model catalog + credentials) currently lives inside the Governance backend: the catalog is the config's `routing` key, and credentials are written via `POST /admin/secrets` to Secrets Manager. If it grows (catalog versioning, multi-cloud connections), extract it into `domains/models/`.

## Build & Deploy
```bash
./build.sh                                   # produces dist/config-api.zip
cd envs/poc
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform init
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform apply
```

## Read the live config
```bash
curl -s "https://<admin-api-id>.execute-api.us-west-2.amazonaws.com/admin/config" \
  -H "x-admin-token: <admin-token>"
```

> Every policy change is **data** (config store), not a deploy. The console is always private (CloudFront/OAC), never a public bucket.
