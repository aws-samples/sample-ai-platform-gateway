# Herança de config: defaults → ORG# → TEAM# → APP#

> Interno AIPlat. Como a config efetiva é resolvida e por que a partição carrega a org.

## Escopos (pk na tabela gov-config)
- `global` — defaults da plataforma.
- `ORG#<org>` — a conta.
- `ORG#<org>#TEAM#<team>` — o time.
- `ORG#<org>#TEAM#<team>#APP#<app>` — o app.

## Config efetiva
O Core mescla a cadeia na ordem **defaults → org → team → app**, com o **mais específico ganhando**. Regra de merge: **mapas mesclam por chave**; **escalares e listas substituem** (por isso `allowed_models`, sendo lista, é substituído inteiro pelo escopo mais específico que a definir).

Hierarquia **progressiva**: níveis ausentes colapsam. `team` vazio resolve para `default`. Config vazia é válida — herda de cima. Isso é o que faz o dev solo e a empresa grande usarem o mesmo código sem bifurcação.

## Alvo de escrita ≠ cadeia de leitura
- Leitura usa a **cadeia** (`ScopeKeys`) para o merge.
- Escrita usa o **escopo único** (`ScopeKey`): org sem time grava em `ORG#`, não em `TEAM#default`. Team-scoped força `TEAM#` (nunca org/global).

## Isolamento estrutural
A **partição carrega a org** (`ORG#<org>…`), então uma leitura nunca cruza orgs, nem por bug. `global` só é escrito por `platform_admin`. O registro de membro é `MEMBER#<org>#<email>` e o de times/apps é `TEAMS#<org>` — prefixos distintos, sem colisão.

## Duplicação de propósito (Core × Governance)
A cadeia de escopo é implementada **duas vezes** (Core e Governance) como **contrato**, não biblioteca compartilhada. O risco de drift é coberto por teste de contrato sobre um fixture comum.
