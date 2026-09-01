# Roteamento, fallback e auto-cheapest

> Interno AIPlat. Como o Core decide qual provedor atende cada requisição.

## Resolução do modelo
1. A API key resolve `org/team/app`. A config efetiva é lida do Governance (BatchGet de `global`, `ORG#`, `ORG#…TEAM#`, `ORG#…TEAM#…APP#`, o mais específico ganha), com cache de ~15s por `org|team|app` e **fallback para defaults** se indisponível.
2. `allowed_models` do escopo filtra o que pode ser usado — modelo fora da lista → 403; auto-cheapest nunca escolhe modelo não permitido.
3. A cadeia de tentativa é montada: 1º ativo = padrão; seguintes = fallback.

## Auto-cheapest respeita a lista
Com `auto_cheapest` ligado, `buildChain` **reordena por preço** apenas os modelos **elegíveis e listados** (mais barato → próximo mais barato), em vez da ordem manual. Elegibilidade = capacidades declaradas (tool use, multimodal, janela de contexto). Assim o fallback também segue "próximo mais barato que serve".

## Fallback
Se o provedor do modelo escolhido falha (timeout, 5xx, indisponível), tenta o próximo da cadeia. Se um segredo de provedor falta, falha **só aquele modelo** e segue. Economia de fallback é contrafactual (baseline = pedido).

## Neutralidade de provedor
Adaptadores: `bedrock`, `openai_compatible` (OpenAI/Azure/Groq/Together/Gemini-compat/self-host), `anthropic` nativo, `google/gemini` nativo. Trocar provedor é config/adaptador — não muda o cliente. Bedrock BYO assume `role_arn` do cliente em runtime (gasto/quota na conta dele).
