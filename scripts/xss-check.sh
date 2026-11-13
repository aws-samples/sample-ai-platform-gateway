#!/usr/bin/env bash
# FE-SEC-001: fail if console.js interpolates a DATA value (a property/index read such as
# ${c.provider}, ${route.role_arn}, ${k.id}) into HTML without esc(). The console renders
# API- and config-supplied values (team names, model aliases, provider-returned ids,
# role_arn) into innerHTML, so every data-derived read must go through esc().
#
# Composed HTML fragments (${badges.join('')}, ${rowsHtml}) and numeric/formatter helpers
# (fmtUSD, when) are safe by construction and are NOT flagged; only a *bare* property/index
# read that is not wrapped in esc()/avVal()/_t() is reported.
set -euo pipefail

JS="$(cd "$(dirname "$0")/.." && pwd)/domains/frontend/site/app/assets/console.js"
[ -f "$JS" ] || { echo "console.js not found at $JS"; exit 2; }

python3 - "$JS" <<'PY'
import re, sys
js = open(sys.argv[1]).read().splitlines()
# A bare data read: ${ ident(.prop|[idx])+ (|| fallback)? } with NO function call inside.
# Method calls (.join(), .map()) and helper calls (esc(), fmtUSD()) contain "(", so a read
# that contains "(" is not bare and is skipped. The optional ||fallback is also required to
# be a bare read for it to count as a violation.
read_re = re.compile(r'\$\{\s*([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*|\[[^\]]*\])+(?:\s*\|\|\s*[A-Za-z_][A-Za-z0-9_.\[\]]*)?)\s*\}')
viol = []
for i, line in enumerate(js, 1):
    for m in read_re.finditer(line):
        expr = m.group(1)
        if '(' in expr:
            continue  # method/helper call, not a bare read
        # numeric-ish fields are safe scalars
        if re.search(r'\.(length|size|count|amount_usd|remaining_usd|consumed_usd)$', expr):
            continue
        viol.append(f"{i}: {m.group(0)}")
if viol:
    print("FE-SEC-001: unescaped data interpolation(s) in console.js:")
    print("\n".join(viol))
    sys.exit(1)
print("xss-check: no bare unescaped data interpolations found")
PY
