# Guardrails deterministas

> Interno AIPlat. Filtros de contenido aplicados en el Core antes de que la solicitud salga.

Se leen de `config.guardrails` (ámbito efectivo) y se aplican **antes** de que la solicitud vaya al proveedor:

- **`mask_pii`** — enmascara correo, identificación fiscal, tarjeta y teléfono en el prompt (regex) antes de enviar.
- **`block_secrets`** — si el prompt contiene una clave, token o secreto, rechaza con `400` (`policy_violation`, code `secret_detected`).
- **`block_injection`** — detección **heurística** de prompt injection (patrones clásicos) → `400` `prompt_injection`. No es un modelo; cubre casos conocidos y no es infalible.
- **`no_store`** — apaga el cache de respuesta de esa org (retención cero): nunca lee ni escribe contenido en el cache.

Los deterministas (mask/secret/no_store) son confiables; **la moderación por modelo todavía no existe**. Un guardrail es **dato** (config), no un deploy — cambia para las próximas solicitudes considerando el cache corto de config (~15s).

Las solicitudes detenidas entran en el Usage_Record como `blocked`, `category = policy`, costo y tokens cero, y aparecen en la pestaña Registros (no en el resumen de costo).
