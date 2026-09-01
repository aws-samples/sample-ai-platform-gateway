## O que a aba resolve
Plano & Faturamento mostra a medição do mês corrente (lida do histórico real), a fatura estimada e a troca de plano. O gasto de LLM é seu (credencial própria); a plataforma cobra a assinatura e, nos planos que têm, uma fração da economia comprovada.

## Como usar
Veja os cartões de consumo e a fatura estimada do mês. Para mudar de plano, escolha o novo tier — os limites (assentos, rate limit, budget) passam a valer como config da sua org.

## Perguntas comuns
- **A fatura é cobrança real?** Não nesta fase — é estimativa a partir do uso medido.
- **O que muda ao subir de plano?** Assentos de membros, limites e o modelo de acesso (por usuário no Pro, por time no Business).

## Por que o saldo de crédito é estimado

Crédito de provedor (AWS Activate, Google Cloud, Azure) é **saldo**, não desconto: o preço por token não muda, muda de qual bolso sai. Enquanto houver saldo, o roteamento prefere o provedor coberto — queimar o crédito antes de expirar é o certo.

O saldo mostrado aqui é **estimado**, e é um **limite inferior** do consumo real. A razão é simples: nós só contamos o que passou pelo gateway, e o crédito do provedor também é consumido por tudo o que você usa fora da plataforma (armazenamento, máquinas, outros serviços).

**O que fazer:** confira o saldo na fatura do provedor e corrija o valor na tela. A correção manual passa a ser a base do cálculo, em vez do valor declarado originalmente.

Quando o crédito expira, o gateway volta a decidir por dinheiro real e o seu budget passa a valer normalmente.
