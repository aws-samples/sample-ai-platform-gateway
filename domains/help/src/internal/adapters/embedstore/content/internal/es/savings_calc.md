# Cálculo de ahorro — cómo se arma el ledger de ROI

> Interno AIPlat. Explica de dónde nace cada `saved_usd` y por qué separamos por fuerza de prueba.

## Dos clases de ahorro
- **Comprobado (`saved_verified_usd`)** — no depende de ningún contrafactual. Hoy es el **cache**: el mismo modelo habría sido llamado y cobrado; como servimos desde el cache, el costo evitado es **observable** (el precio de ese modelo × los tokens que habría usado la respuesta). También entra `provider_prompt_cache` (el proveedor cobró menos por el prefijo). Es la base defendible para gain-share.
- **Contrafactual (`saved_counterfactual_usd`)** — hubo **cambio de modelo**: servido ≠ pedido. Se mide contra el **modelo que el cliente pidió**. Es dinero real, pero solo vale si el pedido reflejaba la intención real. Motivos: `auto_cheapest`, `fallback`, `budget_degrade`.

`saved_verified + saved_counterfactual == saved_usd`.

## Auto-cheapest (el cálculo que genera más dudas)
1. El router recibe una solicitud pidiendo el modelo M (o el modelo predeterminado del orden).
2. Con auto-cheapest activado, elige el **más barato elegible** de la lista de activos (elegibilidad = tool use, multimodal, ventana de contexto). Llamémoslo C.
3. Sirve con C. El costo real es `cost(C) = tokens × precio(C)`.
4. El **baseline** es el modelo **pedido** M: `RequestedCostUSD = tokens_reales × precio(M)` — lo que M habría costado **con los mismos tokens de salida**.
5. `saved = max(0, RequestedCostUSD − cost(C))`, con `savings_reason = auto_cheapest`. Si no hubo cambio (C == M), `saved = 0` — no existe "ahorro fantasma".

El baseline (`requested_model` / `requested_cost_usd`) es **emitido por el router** y **persistido** en el Usage_Record, que es lo que hace el contrafactual **auditable** después.

## Fallback y budget_degrade
- `fallback`: el proveedor del modelo pedido falló; servimos el siguiente de la cadena. Baseline = pedido, misma fórmula.
- `budget_degrade`: el budget se superó con acción `degrade`; forzamos el más barato permitido. Baseline = pedido.

## Crédito ≠ ahorro
`credit_usd` / `cash_usd` particionan el **mismo** `cost_usd` por bolsillo (crédito de proveedor quemado vs desembolso efectivo). **Sumarlos al `cost` contaría dos veces.** El crédito quemado es saldo consumido, no ahorro — por eso "De qué bolsillo salió" lo muestra por separado, para no inflar el ahorro comprobado.

## Precio: lista vs contrato
`cost_list_price_usd` / `cost_contract_price_usd` marcan la procedencia del precio. Si el cliente usa precio de lista y tiene un descuento no registrado, costo y ahorro quedan **por encima** de lo real — `list_price_pct` expone cuánto del período carga ese sesgo.
