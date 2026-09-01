## O que a aba resolve
API Keys emite e revoga as chaves do gateway. Cada chave resolve **org + time + app**: é o que separa seu custo por time e por app. Guardamos só o hash — o valor aparece uma única vez, na criação.

## Como usar
Escolha um time e um app existentes (criados em Times & Apps) e emita. Copie a chave na hora — não mostramos de novo. Aponte o `base_url` do seu app para o gateway e use a chave como Bearer token.

## Perguntas comuns
- **Não consigo criar time/app aqui.** Por desenho: a criação é em Times & Apps; aqui você só associa a chave a algo existente.
- **Chave "de time" ou "de app"?** A chave sempre carrega o time; o app pode ser um específico ou `default` (o time inteiro).
- **Perdi a chave.** Não dá para recuperar (só hash). Revogue e emita outra.
