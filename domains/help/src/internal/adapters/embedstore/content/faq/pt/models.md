## O que a aba resolve
Modelos & Roteamento é onde você conecta provedores e modelos, define a ordem de prioridade (padrão + fallback), liga cache e auto-cheapest. Cada modelo é uma **rota**: um apelido que seu código chama e que aponta para um provedor — trocar o provedor por trás não muda uma linha do seu app.

## Como usar
Na sub-aba **Modelos**, adicione provedores e ligue/desligue cada modelo (tags 🌐 externo / 🏠 interno). Em **Roteamento**, arraste para ordenar os modelos ligados. Em **Configurações**, ligue auto-cheapest e os caches. Preço vem da tabela pública; se você tem desconto, edite para o seu.

## Perguntas comuns
- **Externo × interno?** Externo = provedor SaaS (inclui Bedrock, que roda na sua conta). Interno = endpoint próprio (self-host).
- **Auto-cheapest ignora minha ordem?** Ligado, a ordem de tentativa passa a ser por preço (mais barato → próximo), respeitando elegibilidade (tool use, imagem, contexto).

## Cache exato × cache semântico

São dois mecanismos diferentes, e a diferença importa.

**Cache exato** guarda a resposta e a devolve quando a mesma pergunta volta. A economia é **comprovada**: a chamada ao provedor simplesmente não aconteceu. Não tem risco.

**Cache semântico** vai além e devolve a resposta de uma pergunta *parecida*, comparando o significado. Ele muda o perfil de custo e de risco:

- **Custo de latência:** adiciona ~60 ms nas requisições que **não** acertam o cache, porque a pergunta precisa ser vetorizada.
- **Ganho:** cada acerto evita **100% do custo** daquela chamada. Quanto isso reduz a fatura depende do seu hit-rate, que você acompanha em **ROI & Economia**.
- **Risco:** a resposta é **aproximada, não idêntica**. Ele admite falso positivo.

Por isso ele já vem desligado e a economia dele aparece separada da economia comprovada no ROI.

### Quando NÃO usar cache semântico
Se as suas perguntas se distinguem por **detalhe fino**, o mecanismo tende a errar mais do que ajuda. Exemplos onde a diferença é pequena no texto e grande na resposta: pergunta sobre um time × outro time, um período × outro, uma feature × outra parecida.

Uma proteção já é automática: perguntas com **números diferentes nunca casam** entre si (60 e 600 não se confundem), porque a comparação por significado é fraca justamente com quantidade.

### Rigor (threshold)
O rigor define o quanto duas perguntas precisam ser parecidas para casar. Mais alto = menos acertos, menos risco. Mais baixo = mais acertos, mais chance de resposta aproximada errada. O padrão é conservador de propósito.
