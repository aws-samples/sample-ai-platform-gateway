## O que a aba resolve
Limites & Budget define os tetos que protegem sua fatura e impedem um time barulhento de atrapalhar os outros: rate limit (req/min) e budget mensal em dólar, por escopo (org, time ou app).

## Como usar
Escolha o escopo, defina o rate limit e o budget por degraus, e a ação ao estourar o budget: **alertar**, **degradar** (força o modelo mais barato permitido) ou **bloquear**. As mudanças valem em até ~15s no gateway.

## Perguntas comuns
- **Org, time ou app?** O limite conta no escopo que o definiu: no nível org vale para tudo; no time, só para aquele time.
- **Time ou app — qual eu defino?** Defina no **time** por padrão: todos os apps daquele time compartilham o mesmo teto, sem configuração extra. Só declare um limite direto num **app** quando aquele app específico precisar de um teto diferente do resto do time (ex: um app crítico que nunca deve ser limitado, ou um experimental com teto mais apertado) — o limite no app sobrescreve o do time só para ele, o resto do time não é afetado.
- **Degradar é seguro?** Ele mantém o serviço no ar trocando por um modelo mais barato permitido, em vez de recusar a requisição.
