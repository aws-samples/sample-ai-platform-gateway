# Cache exato e semântico

> Interno AIPlat. Como o gateway reaproveita respostas e por que a chave inclui a org.

## Cache exato
- **Chave:** `sha256(org | model | messages)` — a org entra na chave, então **nunca** há hit cross-tenant (isolamento estrutural, verificado por teste).
- **TTL:** `config.cache_ttl` (0 = desligado). Hit exato = economia **verificada** (mesmo modelo, custo evitado observável), `savings_reason = cache`.
- `no_store` (guardrail) desliga o cache daquela org (retenção zero): nunca lê nem grava.

## Cache semântico (GPTCache-style, "nosso jeito")
- Opt-in (`semantic_cache`), independente do exato — pode ligar só ele.
- Após miss exato/canônico, se ligado: **embed** da pergunta (Titan v2, dim 256) → busca no índice por **cosseno** → serve se similaridade ≥ `semantic_threshold` (0.92 equilibrado; 0.95 preciso; 0.88 agressivo).
- **Índice por tenant:** um único item `SEMIDX#<org>` na tabela de cache (zero infra nova). Vetores quantizados int8 para caber.
- Economia entra como `semantic_cache` (**contrafactual/aproximada** — admite falso positivo). Custo/latência: ~60ms de vetorização por miss; cada hit evita 100% do custo da chamada.

## Prompt caching do provedor (cache de prefixo)
Por rota (`prompt_cache`), só onde o provedor suporta (Bedrock/Anthropic): o gateway marca o fim do system para cachear o prefixo estável; leituras seguintes com o mesmo prefixo custam ~90% menos (a 1ª gravação cobra um prêmio). Entra como economia verificada `provider_prompt_cache`.
