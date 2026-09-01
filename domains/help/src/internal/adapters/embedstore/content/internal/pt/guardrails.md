# Guardrails determinísticos

> Interno AIPlat. Filtros de conteúdo aplicados no Core antes de a requisição sair.

Lidos de `config.guardrails` (escopo efetivo) e aplicados **antes** de a requisição ir ao provedor:

- **`mask_pii`** — mascara e-mail/CPF/cartão/telefone no prompt (regex) antes de enviar.
- **`block_secrets`** — se o prompt contém chave/token/segredo, recusa com `400` (`policy_violation`, code `secret_detected`).
- **`block_injection`** — **heurística** de prompt injection (padrões clássicos) → `400` `prompt_injection`. Não é modelo; cobre casos conhecidos, não é infalível.
- **`no_store`** — desliga o cache de resposta daquela org (retenção zero): nunca lê nem grava conteúdo no cache.

Determinísticos (mask/secret/no_store) são confiáveis; **moderação por modelo ainda não existe**. Guardrail é **dado** (config), não deploy — muda para as próximas requisições respeitando o cache curto de config (~15s).

Requisições barradas entram no Usage_Record como `blocked`, `category = policy`, custo/tokens zero, e aparecem na aba Logs (não no resumo de custo).
