# SLI/SLO: Wilson, shrinkage y burn rate

> Interno AIPlat. Cómo medimos confiabilidad sin entrar en pánico con poco volumen.

## SLI de disponibilidad
`availability = bueno / elegible`, donde **bueno** = servido + cache y **elegible** = un éxito **o** una falla que cuenta contra nosotros (`sli_eligible=true`). Quedan **fuera**: política (rate/budget/guardrail/suspensión), config y auth del cliente, y **la cuota del proveedor del cliente** (`provider_quota_exceeded`). **Cuentan**: `provider_unreachable`/`provider_down` (dependencia) y `platform`/`auth_backend_error` (nosotros o AWS). Solo entra `mode: sync` (el batch tiene latencia de horas y destruiría el error budget).

## Sensible al volumen (evita falso pánico)
- **Piso de volumen (20):** por debajo → `insufficient_data`.
- **Intervalo de Wilson:** confianza sobre la tasa observada.
- **Shrinkage bayesiano (`adjusted_pct`):** media ponderada observado × baseline del tier (peso `k=50`). Así 1 falla en 2 llamadas no se convierte en breach, y 2% sobre 1M se detecta con rigor.
- **Estados:** `healthy` / `at_risk` (ajustado < objetivo) / `breaching` (límite superior de Wilson < objetivo, o sea con confianza) / `insufficient_data`.
- **Objetivo por tier:** free 99 / pro 99,5 / business 99,9.

## Burn rate (SLO) — alert-notifier
`burn = tasa_de_error / (1 − SLO)`. Multi-ventana (SRE workbook): **fast burn** 14,4× en 1h → `page`; **slow burn** 6× en 6h → `ticket`. Un piso de volumen por ventana evita ruido. Alerta por la **amenaza al error budget**, no por un error aislado.

## Anomalía vs el baseline del cliente
Aprende la tasa de error **normal del propio cliente** (7 días, excluyendo la última hora) y alerta cuando la última hora se desvía por **z-score ≥ 3** (binomial). Guardas: baseline ≥ 50 elegibles, volumen actual ≥ 10, tasa ≥ max(2× baseline, 2%) y ≥ 3 errores. Detecta un spike incluso mientras el SLO global sigue viéndose bien.
