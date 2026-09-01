## O que a aba resolve
Guardrails são filtros de segurança de conteúdo aplicados **antes** de a requisição sair para o provedor: mascarar PII, bloquear segredos, barrar tentativas de prompt injection e desligar o armazenamento de cache (retenção zero).

## Como usar
Ligue por escopo os guardrails desejados. Eles valem para as próximas requisições (respeitando o cache curto de config). Requisições barradas aparecem em Logs como "bloqueado".

## Perguntas comuns
- **É infalível?** Os determinísticos (PII, segredo, no_store) são confiáveis. A detecção de injection é heurística — cobre casos conhecidos, não tudo.
- **Moderação por modelo?** Ainda não existe; hoje os guardrails são baseados em regra.
