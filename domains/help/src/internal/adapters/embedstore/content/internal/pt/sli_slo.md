# SLI/SLO: Wilson, shrinkage e burn rate

> Interno AIPlat. Como medimos confiabilidade sem pânico com pouco volume.

## SLI de disponibilidade
`availability = bom / elegível`, onde **bom** = servido + cache e **elegível** = sucesso **ou** falha que conta contra nós (`sli_eligible=true`). Ficam **de fora**: política (rate/budget/guardrail/suspensão), config/auth do cliente e **quota do provedor do cliente** (`provider_quota_exceeded`). **Contam**: `provider_unreachable`/`provider_down` (dependência) e `platform`/`auth_backend_error` (nós/AWS). Só `mode: sync` entra (batch tem latência de horas e destruiria o error budget).

## Sensível a volume (evita falso pânico)
- **Piso de volume (20):** abaixo disso → `insufficient_data`.
- **Intervalo de Wilson:** confiança sobre a taxa observada.
- **Shrinkage bayesiano (`adjusted_pct`):** média ponderada observado × baseline do tier (peso `k=50`). Assim 1 falha em 2 chamadas não vira breach, e 2% em 1M é pego com rigor.
- **Estados:** `healthy` / `at_risk` (ajustado < alvo) / `breaching` (Wilson-alto < alvo, confiante) / `insufficient_data`.
- **Alvo por tier:** free 99 / pro 99,5 / business 99,9.

## Burn rate (SLO) — alert-notifier
`burn = taxa_de_erro / (1 − SLO)`. Multi-janela (SRE workbook): **fast burn** 14,4× em 1h → `page`; **slow burn** 6× em 6h → `ticket`. Piso de volume por janela evita ruído. Alerta pela **ameaça ao error budget**, não por erro isolado.

## Anomalia vs baseline do cliente
Aprende a taxa de erro **normal do próprio cliente** (7 dias, excluindo a última hora) e alerta quando a última hora desvia por **z-score ≥ 3** (binomial). Guardas: baseline ≥50 elegíveis, volume atual ≥10, taxa ≥ max(2×baseline, 2%) e ≥3 erros. Pega spike mesmo dentro do SLO global.
