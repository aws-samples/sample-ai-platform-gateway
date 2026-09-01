# Rate limit y budget

> Interno AIPlat. Cómo se aplican los guardrails de factura y de vecindad.

## Dónde viven los contadores
En la tabla propia del Core (`*-limits`, con TTL) — **no** hay lectura sincrónica del Cost_Store de Observability (eso preserva el aislamiento entre dominios). El Cost_Store es la fuente histórica y autoritativa; los contadores son un guardrail operacional (una aproximación).

## Ámbito del límite
El contador usa el ámbito que **definió** la política. Un límite en la org cuenta para toda la org; en el equipo, solo para ese equipo (herencia `TEAM# → APP#`).

## Rate limit
Ventana fija de un minuto. `requests_per_minute` usa un `ADD` atómico en DynamoDB (bloquea al exceder); `tokens_per_minute` se acumula después de la respuesta y detiene la solicitud *siguiente*. La respuesta es `429` en formato de error de OpenAI (`rate_limit_exceeded`) con `retry-after`.

## Budget mensual
Gasto acumulado por ámbito y mes. Acciones:
- **`alert`** — sigue sirviendo y marca `budget_state`.
- **`degrade`** — fuerza el modelo más barato permitido (genera ahorro contrafactual `budget_degrade`).
- **`block`** — `429` `insufficient_quota`.

El campo `aiplat.budget_state` en la respuesta expone el estado (`""`, `exceeded_alert`, `exceeded_degraded`).

## Propagación
Un cambio de política vale en hasta ~15s (cache de config por instancia de Lambda).
