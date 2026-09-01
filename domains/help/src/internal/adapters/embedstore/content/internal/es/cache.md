# Cache exacto y semántico

> Interno AIPlat. Cómo el gateway reutiliza respuestas y por qué la clave incluye la org.

## Cache exacto
- **Clave:** `sha256(org | model | messages)` — la org forma parte de la clave, así que un acierto cross-tenant es **imposible** (aislamiento estructural, verificado por test).
- **TTL:** `config.cache_ttl` (0 = apagado). Un acierto exacto es ahorro **comprobado** (mismo modelo, el costo evitado es observable), `savings_reason = cache`.
- `no_store` (guardrail) apaga el cache de esa org (retención cero): no lee ni escribe.

## Cache semántico (estilo GPTCache, a nuestra manera)
- Opt-in (`semantic_cache`), independiente del exacto — puedes activar solo este.
- Tras un miss exacto/canónico, si está activo: **embed** de la pregunta (Titan v2, dim 256) → búsqueda en el índice por **coseno** → sirve cuando la similitud ≥ `semantic_threshold` (0.92 equilibrado; 0.95 preciso; 0.88 agresivo).
- **Índice por tenant:** un único ítem `SEMIDX#<org>` en la tabla de cache (cero infraestructura nueva). Los vectores se cuantizan a int8 para que quepan.
- El ahorro se registra como `semantic_cache` (**contrafactual/aproximado** — admite falsos positivos). Costo y latencia: ~60 ms de vectorización por miss; cada acierto evita 100% del costo de esa llamada.

## Prompt caching del proveedor (cache de prefijo)
Por ruta (`prompt_cache`), solo donde el proveedor lo soporta (Bedrock/Anthropic): el gateway marca el fin del system para cachear el prefijo estable; las lecturas siguientes con el mismo prefijo cuestan ~90% menos (la 1ª escritura cobra un premio). Se registra como ahorro comprobado `provider_prompt_cache`.
