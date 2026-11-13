# Domain: Governance & Config (Settings tab)

Source of truth for dynamic config, membership and provider credentials. It's where an admin changes behavior (auto-cheapest, routing, pricing, cache) **without a redeploy**, and where members, teams and apps are managed.

## Structure (all owned by the domain)
```
governance/
├── build.sh              # compiles the Go Lambdas (arm64)
├── build_config.sh       # packages the seed default_config.json
├── src/                  # Go code
│   ├── go.mod
│   └── cmd/
│       ├── config-api/   # ~25 admin routes: /admin/config, /admin/members,
│       │                 #   /admin/teams, /admin/apps, /admin/secrets, …
│       └── pretoken/     # Cognito pre-token-generation trigger (injects claims)
├── dist/                 # .zip artifacts (bootstrap)
├── tf/                   # Terraform module (API, Lambdas, Cognito pool, tables)
└── envs/poc/             # per-env Terraform root + default_config.json (seed)
```

> The web console is no longer served from this domain. The frontend lives in the
> frontend domain and is served via its own private S3 + CloudFront/OAC.

## AWS resources (poc)
- Lambda (Go, `provided.al2023`, arm64): `aiplat-poc-gov-config-api` and the pre-token trigger
- DynamoDB: `aiplat-poc-gov-config` — one item per **scope / entity**, not a single row:
  - policy scopes `pk="global"`, `pk="ORG#<org>"`, `pk="ORG#<org>#TEAM#<team>"`, `pk="ORG#<org>#TEAM#<team>#APP#<app>"`
  - membership/registry `pk="MEMBER#…"`, `pk="TEAMS#<org>"`
- Admin API (**REST API**, API Gateway): `https://<admin-api-id>.execute-api.us-west-2.amazonaws.com/<stage>`
- Cognito user pool (control-plane sign-in) with a `COGNITO_USER_POOLS` authorizer on the API
- Secrets Manager: provider credentials under `aiplat/gateway/*` and per-org BYO keys under `aiplat/org/<org>/<provider>`

**Auth:** requests carry a Cognito JWT in the `Authorization` header; API Gateway's
`COGNITO_USER_POOLS` authorizer validates it. The token's `custom:org_id`,
`custom:role` and `team` claims drive org scope and role checks inside the handler.
The old PoC `x-admin-token` header was removed in 1.5.0 (it survives only in
CHANGELOG history).

## Contracts (EDA boundaries)
- **Produces (→ Core):** config read at runtime by Core (with fallback). Changes apply to subsequent requests.
- **Produces (→ Observability):** budget thresholds/policies that generate Cost_Events.
- **Produces (→ Audit):** control-plane audit events on the Audit EventBridge bus (`AUDIT_BUS`).

## Models domain (folded in here)
The logical **Models & Providers** domain (model catalog + credentials) currently lives inside the Governance backend: the catalog is the config's `routing` key, and credentials are written via `POST /admin/secrets` to Secrets Manager. If it grows (catalog versioning, multi-cloud connections), extract it into `domains/models/`.

## Build & Deploy
```bash
./build.sh                                   # produces dist/*.zip
cd envs/poc
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform init
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform apply
```

## Read the live config
Obtain a Cognito ID token for an admin user, then:
```bash
curl -s "https://<admin-api-id>.execute-api.us-west-2.amazonaws.com/<stage>/admin/config" \
  -H "authorization: <cognito-id-token>"
```

> Every policy change is **data** (config store), not a deploy. The console is always private (CloudFront/OAC), never a public bucket.
