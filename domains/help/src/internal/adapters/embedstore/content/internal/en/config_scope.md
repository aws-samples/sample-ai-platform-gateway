# Config inheritance: defaults → ORG# → TEAM# → APP#

> AIPlat internal. How the effective config is resolved and why the partition carries the org.

## Scopes (pk in the gov-config table)
- `global` — platform defaults.
- `ORG#<org>` — the account.
- `ORG#<org>#TEAM#<team>` — the team.
- `ORG#<org>#TEAM#<team>#APP#<app>` — the app.

## Effective config
The Core merges the chain in the order **defaults → org → team → app**, with the **most specific winning**. Merge rule: **maps merge per key**; **scalars and lists replace** (which is why `allowed_models`, being a list, is replaced wholesale by the most specific scope that defines it).

The hierarchy is **progressive**: absent levels collapse. An empty `team` resolves to `default`. An empty config is valid — it inherits from above. That is what lets the solo dev and the large enterprise run the same code with no branching.

## Write target ≠ read chain
- Reads use the **chain** (`ScopeKeys`) for the merge.
- Writes use a **single scope** (`ScopeKey`): an org with no team writes to `ORG#`, not to `TEAM#default`. Team-scoped callers are forced to `TEAM#` (never org or global).

## Structural isolation
The **partition carries the org** (`ORG#<org>…`), so a read never crosses orgs, not even through a bug. `global` is written only by `platform_admin`. The member record is `MEMBER#<org>#<email>` and the teams/apps record is `TEAMS#<org>` — distinct prefixes, no collision.

## Duplication on purpose (Core × Governance)
The scope chain is implemented **twice** (Core and Governance) as a **contract**, not a shared runtime library. The drift risk is covered by a contract test against a common fixture.
