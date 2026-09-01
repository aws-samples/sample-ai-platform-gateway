# Enrutamiento, fallback y auto-cheapest

> Interno AIPlat. Cómo el Core decide qué proveedor atiende cada solicitud.

## Resolución del modelo
1. La API key resuelve `org/team/app`. La config efectiva se lee del Governance (BatchGet de `global`, `ORG#`, `ORG#…TEAM#`, `ORG#…TEAM#…APP#`, gana el más específico), con cache de ~15s por `org|team|app` y **fallback a los defaults** si no está disponible.
2. `allowed_models` del ámbito filtra lo que puede usarse — un modelo fuera de la lista → 403; el auto-cheapest nunca elige un modelo no permitido.
3. Se arma la cadena de intento: 1º activo = predeterminado; los siguientes = fallback.

## El auto-cheapest respeta la lista
Con `auto_cheapest` activado, `buildChain` **reordena por precio** solo los modelos **elegibles y listados** (más barato → siguiente más barato), en vez del orden manual. Elegibilidad = capacidades declaradas (tool use, multimodal, ventana de contexto). Así el fallback también sigue "el siguiente más barato que sirve".

## Fallback
Si el proveedor del modelo elegido falla (timeout, 5xx, no disponible), se intenta el siguiente de la cadena. Si falta un secreto de proveedor, falla **solo ese modelo** y la cadena continúa. El ahorro por fallback es contrafactual (baseline = el modelo pedido).

## Neutralidad de proveedor
Adaptadores: `bedrock`, `openai_compatible` (OpenAI/Azure/Groq/Together/Gemini-compat/self-host), `anthropic` nativo, `google/gemini` nativo. Cambiar de proveedor es config más adaptador — el cliente no cambia. Bedrock BYO asume el `role_arn` del cliente en runtime (gasto y cuota quedan en su cuenta).
