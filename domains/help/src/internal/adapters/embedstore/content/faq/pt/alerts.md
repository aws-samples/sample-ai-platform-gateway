## O que a aba resolve
Alertas define regras avaliadas contra o uso real (gasto, latência, cache baixo, taxa de erro, capacidade do provedor) e um webhook para receber o aviso quando uma regra dispara.

## Como usar
Ligue as regras que importam e defina o limiar de cada uma. Informe um webhook (URL sua) para receber o POST. Um avaliador roda no servidor a cada 15 min e entrega no webhook, no máximo uma vez por regra por dia.

## Perguntas comuns
- **A regra de consumo do plano é entregue?** Ela é avaliada ao vivo aqui, mas ainda não é entregue por webhook.
- **Por que não disparou?** Regras de erro têm piso de volume por janela para evitar ruído com poucas requisições.

## Como a entrega funciona (e o que ainda não é entregue)

Um avaliador roda no servidor a cada **15 minutos**, confere as regras ligadas e faz um POST no seu webhook quando alguma dispara. O disparo é limitado a **uma vez por regra por dia** (cooldown), para não transformar um problema contínuo em enxurrada de avisos.

**Entregues por webhook:** gasto do mês, latência média, cache baixo, taxa de erro, capacidade do provedor, queima de error budget (SLO) e anomalia versus o seu próprio histórico.

**Avaliada só ao vivo nesta tela:** consumo da cota do plano. Ela depende do catálogo de planos, que é de outro domínio, e ainda não é entregue por webhook.

## Um alerta pode disparar e não chegar

Se o seu webhook estiver fora do ar ou recusar a chamada, o aviso se perde — e **não é reenviado**, porque o cooldown do dia já foi consumido.

É por isso que existe o histórico em **Logs → Alertas**: lá você vê cada disparo, o valor que o acionou e se a entrega funcionou. Se aparecer "não entregue", confira a URL do webhook aqui. Guardamos apenas o domínio do endereço, nunca a URL completa, porque ela costuma conter token.
