# Taxonomía de fallas (reason / category / sli_eligible)

> Interno AIPlat. Cómo el Core clasifica cada solicitud y qué cuenta en el SLI.

## Status en el Usage_Record
`status` ∈ `success` | `error` | `blocked` (vacío = success, registro antiguo). El router emite Usage_Record **también en error y bloqueo** (costo y tokens cero, reutilizando el `request_id` del API Gateway). El resumen de costo (`/usage/summary`) **excluye** error y bloqueo para no distorsionar conteo ni latencia — esos viven en Registros.

## Campos de clasificación (emitidos por el Core, el único que conoce la causa)
- **`reason`** — código corto: `rate_limit_exceeded`, `budget_exceeded`, `secret_detected`, `prompt_injection`, `model_not_allowed`, `unknown_model`, `invalid_body`, `account_suspended`, `provider_quota_exceeded`, `provider_auth`, `provider_rate_limited`, `provider_unreachable`, `provider_down`, `provider_error`, `auth_backend_error`.
- **`category`** (FCAPS) — `config` · `auth` · `policy` · `dependency` · `platform` · `capacity` · `ok`.
- **`sli_eligible`** (bool) — si cuenta en el SLI de confiabilidad de la plataforma. **Solo cuentan NUESTRAS fallas.**
- **`detail`** — texto corto, solo para error de proveedor (nunca contiene el prompt), truncado a 300 caracteres.

## Qué cuenta en el SLI
- **Fuera** (no es falla nuestra): política (rate/budget/guardrail/suspensión), config y auth del cliente, y **la cuota del proveedor del cliente** (`provider_quota_exceeded`).
- **Dentro:** `provider_unreachable`/`provider_down` (dependencia) y `platform`/`auth_backend_error` (nosotros o AWS).

## Matiz SLA vs SLI
`provider_quota_exceeded` (el saldo o la cuota del cliente en el proveedor se agotó) queda **fuera del SLI**, pero dispara una **alerta crítica de capacidad** — el cliente se quedó sin de dónde consumir, lo que no es falla nuestra pero sí es su problema.
