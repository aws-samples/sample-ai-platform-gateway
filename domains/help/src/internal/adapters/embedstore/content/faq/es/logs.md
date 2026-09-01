## Para qué sirve esta pestaña
Registros trae una fila por solicitud del período — modelo, proveedor, tokens, costo, latencia, cache y resultado (servido, cache, error de proveedor, bloqueado por política). Es la inspección fina de lo que pasó por el gateway.

## Cómo usarla
Filtra por período, resultado y modelo. Las solicitudes bloqueadas y con error cuestan cero y sí aparecen aquí, pero quedan fuera del resumen de costo (que solo cuenta lo que fue servido).

## Preguntas frecuentes
- **¿Puedo ver el prompt o la respuesta?** No. Por política el contenido nunca se almacena — guardamos solo metadatos y un hash usado como clave de cache.
- **¿Qué significa "bloqueado"?** La solicitud fue detenida por un guardrail, rate limit, budget, una cuenta suspendida o un modelo no permitido.
