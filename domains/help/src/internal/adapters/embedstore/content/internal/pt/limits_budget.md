# Rate limit e budget

> Interno AIPlat. Como os guardrails de fatura/vizinhança são aplicados.

## Onde vivem os contadores
Tabela própria do Core (`*-limits`, com TTL) — **não** há leitura síncrona no Cost_Store do Observability (preserva isolamento entre domínios). O Cost_Store é a fonte histórica/autoritativa; os contadores são guardrail operacional (aproximação).

## Escopo do limite
O contador usa o escopo que **definiu** a política. Limite na org conta para a org inteira; no time, só para aquele time (herança `TEAM# → APP#`).

## Rate limit
Janela fixa de 1 minuto. `requests_per_minute` via `ADD` atômico no DynamoDB (bloqueia ao exceder); `tokens_per_minute` é acumulado pós-resposta e barra a requisição seguinte. Resposta `429` no formato de erro OpenAI (`rate_limit_exceeded`) com `retry-after`.

## Budget mensal
Gasto acumulado por escopo/mês. Ações:
- **`alert`** — segue e marca `budget_state`.
- **`degrade`** — força o modelo mais barato permitido (gera economia contrafactual `budget_degrade`).
- **`block`** — `429` `insufficient_quota`.

O campo `aiplat.budget_state` na resposta expõe o estado (`""`, `exceeded_alert`, `exceeded_degraded`).

## Propagação
Mudança de política vale em até ~15s (cache de config por instância de Lambda).
