# Herencia de config: defaults → ORG# → TEAM# → APP#

> Interno AIPlat. Cómo se resuelve la config efectiva y por qué la partición lleva la org.

## Ámbitos (pk en la tabla gov-config)
- `global` — defaults de la plataforma.
- `ORG#<org>` — la cuenta.
- `ORG#<org>#TEAM#<team>` — el equipo.
- `ORG#<org>#TEAM#<team>#APP#<app>` — la app.

## Config efectiva
El Core mezcla la cadena en el orden **defaults → org → equipo → app**, y gana el **más específico**. Regla de merge: **los mapas se mezclan por clave**; **los escalares y las listas reemplazan** (por eso `allowed_models`, al ser lista, es reemplazado por completo por el ámbito más específico que la defina).

La jerarquía es **progresiva**: los niveles ausentes colapsan. Un `team` vacío resuelve a `default`. Una config vacía es válida — hereda de arriba. Eso es lo que permite que el dev solo y la empresa grande usen el mismo código sin bifurcación.

## Destino de escritura ≠ cadena de lectura
- La lectura usa la **cadena** (`ScopeKeys`) para el merge.
- La escritura usa un **ámbito único** (`ScopeKey`): una org sin equipo escribe en `ORG#`, no en `TEAM#default`. Los llamadores restringidos a un equipo quedan forzados a `TEAM#` (nunca org ni global).

## Aislamiento estructural
La **partición lleva la org** (`ORG#<org>…`), así que una lectura nunca cruza organizaciones, ni por un bug. `global` solo lo escribe `platform_admin`. El registro de miembro es `MEMBER#<org>#<email>` y el de equipos/apps es `TEAMS#<org>` — prefijos distintos, sin colisión.

## Duplicación a propósito (Core × Governance)
La cadena de ámbitos está implementada **dos veces** (Core y Governance) como **contrato**, no como librería compartida en runtime. El riesgo de drift está cubierto por un test de contrato contra un fixture común.
