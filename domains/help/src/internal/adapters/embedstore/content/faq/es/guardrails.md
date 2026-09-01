## Para qué sirve esta pestaña
Los guardrails son filtros de seguridad de contenido aplicados **antes** de que la solicitud salga al proveedor: enmascarar PII, bloquear secretos, detener intentos de prompt injection y desactivar el almacenamiento en cache (retención cero).

## Cómo usarla
Activa por ámbito los guardrails que quieras. Valen para las próximas solicitudes (considerando el cache corto de config). Las solicitudes detenidas aparecen en Registros como "bloqueado".

## Preguntas frecuentes
- **¿Es infalible?** Los deterministas (PII, secretos, no_store) son confiables. La detección de injection es heurística — cubre casos conocidos, no todo.
- **¿Moderación por modelo?** Todavía no existe; hoy los guardrails son basados en reglas.
