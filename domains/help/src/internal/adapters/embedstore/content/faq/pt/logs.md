## O que a aba resolve
Logs traz uma linha por requisição do período — modelo, provedor, tokens, custo, latência, cache e resultado (servido, cache, erro de provedor, bloqueado por política). É a inspeção fina do que passou pelo gateway.

## Como usar
Filtre por período, resultado e modelo. Requisições bloqueadas e com erro têm custo zero e aparecem aqui, mas não entram no resumo de custo (que só conta o que foi servido).

## Perguntas comuns
- **Consigo ver o prompt/resposta?** Não. Por política, o conteúdo nunca é armazenado — guardamos só metadados e um hash como chave de cache.
- **O que é "bloqueado"?** A requisição foi barrada por guardrail, rate limit, budget, conta suspensa ou modelo não permitido.
