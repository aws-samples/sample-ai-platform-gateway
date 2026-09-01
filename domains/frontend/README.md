# Frontend Domain — console

Its own stack (isolated state) that serves the platform front-end: the logged-in **console** (`console.html`). Published as static content on **private S3 + CloudFront/OAC** — no server, no build step.

> No public sales landing page — it was removed from the project. The console is the only surface and may, optionally, sit behind your own authentication proxy at the edge.

Why it's a separate domain: the front-end is a **client** of every other domain (core, observability, governance). It belongs to none of them. Having its own stack gives it an independent lifecycle and deployment (the front-end changes far more often than the backend) and reduces blast radius — republishing a screen doesn't touch the Terraform that holds the config-api or Cognito.

## Layout

```
domains/frontend/
├── site/
│   ├── app/console.html    # logged-in console (static SPA) — default_root_object
│   └── env.js.tftpl        # endpoints injected by Terraform
└── envs/poc/               # Terraform: S3 + CloudFront/OAC (isolated state)
```

There is no `src/`/`build.sh`: the domain has no Go backend. It's just static content + IaC.

## Inbound contracts (endpoints the console consumes)

The front-end doesn't talk to the other domains at build time (isolated state). The endpoints come in as **Terraform variables** (with defaults = live values) and are injected into `env.js` (`window.AIPLAT`). The console reads `window.AIPLAT` at runtime and calls each API over HTTP — exactly as an external client would.

- `admin_api_endpoint` (governance): config, signup, billing, secrets
- `gateway_endpoint` (core): the Playground's `/v1/chat/completions`
- `usage_api_endpoint` (observability): Costs & Usage, ROI
- `keyadmin_endpoint` (core): API Keys
- `cognito_client_id` + `region` (governance): login/signup

## Deploy

```bash
cd domains/frontend/envs/poc
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform init
TF_PLUGIN_CACHE_DIR=~/.terraform.d/plugin-cache AWS_REGION=us-west-2 terraform apply
```

Changing a screen = edit the HTML and `terraform apply` (the S3 objects use `no-cache`, so the change appears without an invalidation).

## Rules

- **No hardcoded endpoints** in the HTML — everything comes from `window.AIPLAT`.
- **No AWS SDK in the browser**; Cognito login is a plain HTTP POST.
