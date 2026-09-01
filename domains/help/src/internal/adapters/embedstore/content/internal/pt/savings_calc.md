# Cálculo de economia — como o ledger de ROI é montado

> Interno AIPlat. Explica como cada `saved_usd` nasce e por que separamos por força de prova.

## Duas classes de economia
- **Verificada (`saved_verified_usd`)** — não depende de contrafactual. Hoje é o **cache**: o mesmo modelo teria sido chamado e cobrado; como servimos do cache, o custo evitado é **observável** (o preço daquele modelo × tokens que a resposta teria). Também entra `provider_prompt_cache` (o provedor cobrou menos pelo prefixo). É a base defensável para gain-share.
- **Contrafactual (`saved_counterfactual_usd`)** — houve **troca de modelo**: servido ≠ pedido. Mede-se contra o **modelo que o cliente pediu**. É real em dinheiro, mas só vale se o pedido era a intenção real. Motivos: `auto_cheapest`, `fallback`, `budget_degrade`.

`saved_verified + saved_counterfactual == saved_usd`.

## Auto-cheapest (o cálculo que mais gera dúvida)
1. O router recebe a requisição pedindo o modelo M (ou o modelo padrão da ordem).
2. Com auto-cheapest ligado, ele escolhe o **mais barato elegível** da lista de ligados (elegibilidade = tool use, multimodal, janela de contexto). Chame de C.
3. Serve com C. O custo real é `cost(C) = tokens × preço(C)`.
4. O **baseline** é o modelo **pedido** M: `RequestedCostUSD = tokens_reais × preço(M)` — o que M teria custado **nos mesmos tokens de saída**.
5. `saved = max(0, RequestedCostUSD − cost(C))`, com `savings_reason = auto_cheapest`. Se não houve troca (C == M), `saved = 0` — não existe "economia fantasma".

O baseline (`requested_model` / `requested_cost_usd`) é **emitido pelo router** e **persistido** no Usage_Record, o que torna o contrafactual **auditável** depois.

## Fallback e budget_degrade
- `fallback`: o provedor do modelo pedido falhou; servimos o próximo da cadeia. Baseline = pedido, mesma fórmula.
- `budget_degrade`: o budget estourou com ação `degrade`; forçamos o mais barato permitido. Baseline = pedido.

## Crédito ≠ economia
`credit_usd` / `cash_usd` particionam o **mesmo** `cost_usd` por bolso (crédito de provedor queimado vs desembolso). **Somar com `cost` contaria duas vezes.** Crédito queimado é saldo consumido, não economia — por isso "De qual bolso saiu" mostra separado, para não inflar a economia comprovada.

## Preço: tabela vs contrato
`cost_list_price_usd` / `cost_contract_price_usd` marcam a procedência do preço. Se o cliente usa preço de tabela e tem desconto não cadastrado, custo e economia ficam **acima** do real — `list_price_pct` expõe quanto do período tem esse viés.
