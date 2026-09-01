#!/usr/bin/env bash
# Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
# SPDX-License-Identifier: MIT-0
# i18n-check.sh — guards for the language policy (.kiro/steering/aiplat-language-policy.md)
#
# Shell only, on purpose: no Python (it hangs in this environment) and no Node
# (not reliably available). Every check is grep/sed/awk.
#
# Usage:  scripts/i18n-check.sh [--quiet]
# Exit:   0 = all checks passed, 1 = at least one finding
#
# What it CANNOT do: parse JavaScript. It will not catch unbalanced quotes or
# brackets. The last line of defense is still opening the screen.

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CONSOLE="$ROOT/domains/frontend/site/app/console.html"
QUIET="${1:-}"
FAIL=0

say() { [ "$QUIET" = "--quiet" ] || printf '%s\n' "$*"; }
hdr() { say ""; say "── $* ──"; }

[ -f "$CONSOLE" ] || { echo "console.html not found at $CONSOLE"; exit 1; }

# ── 1. Duplicate top-level declarations ───────────────────────────────────────
# A duplicated `const` is a SyntaxError, and a SyntaxError in an inline script
# kills the ENTIRE script. This already broke login once (ROLE_LABEL declared
# twice). get_diagnostics on an HTML file does not parse inline JS, so this grep
# is the only cheap guard that catches it.
hdr "1. Duplicate top-level declarations"
DUPES="$(grep -oE '^(const|let|function) +[A-Za-z_$][A-Za-z0-9_$]*' "$CONSOLE" \
         | awk '{print $2}' | sort | uniq -d)"
if [ -n "$DUPES" ]; then
  say "FAIL — declared more than once at top level:"
  say "$DUPES" | sed 's/^/    /'
  FAIL=1
else
  say "ok — no collisions"
fi

# ── 1b. Real syntax check of every inline <script> ────────────────────────────
# Check 1 is a grep and only catches duplicate declarations. This parses the inline
# JS for real, so an unbalanced quote or bracket fails here instead of on screen.
# get_diagnostics on an HTML file does NOT parse inline JS, so without this the
# only signal is a blank console. Skipped when node is absent.
hdr "1b. Inline script syntax"
# Both consoles are checked, not just the translated one. The operator console
# (backoffice) has no dictionaries and no other gate at all, so a stray quote
# there ships a blank page to the person whose job is to notice outages.
#
# An ARRAY, not a space-separated string: the repo path contains spaces
# ("My Documents"), so word splitting turned two files into six bogus ones — and
# awk failing to open them still let the check print "ok". A guard that reports
# success while inspecting nothing is worse than no guard, so the count of files
# checked is printed and the loop counts only real parses.
HTML_FILES=("$CONSOLE")
OPCONSOLE="$ROOT/domains/backoffice/site/app/index.html"
[ -f "$OPCONSOLE" ] && HTML_FILES+=("$OPCONSOLE")
if command -v node >/dev/null 2>&1; then
  TMPD="$(mktemp -d)"
  for html in "${HTML_FILES[@]}"; do
    # Pair up the <script> ... </script> line numbers, skipping tags with src=.
    awk 'index($0,"<script")&&!index($0,"src=")&&!index($0,"</script>"){open=NR;next}
         index($0,"</script>")&&open{print open+1","NR-1;open=0}' "$html" \
    | while IFS= read -r range; do
        n="${range%%,*}"
        sed -n "${range}p" "$html" > "$TMPD/$n.js"
        if OUT="$(node --check "$TMPD/$n.js" 2>&1)"; then
          echo x >> "$TMPD/parsed"
        else
          say "FAIL — syntax error in $(basename "$html"), script starting at line $n:"
          printf '%s\n' "$OUT" | head -6 | sed 's/^/    /'
          echo fail > "$TMPD/failed"
        fi
      done
  done
  PARSED="$(grep -c . "$TMPD/parsed" 2>/dev/null || echo 0)"
  if [ -f "$TMPD/failed" ]; then
    FAIL=1
  elif [ "$PARSED" -lt "${#HTML_FILES[@]}" ]; then
    # Fewer blocks parsed than files inspected means the extraction itself broke.
    say "FAIL — only $PARSED script blocks parsed across ${#HTML_FILES[@]} files; extraction is broken"
    FAIL=1
  else
    say "ok — $PARSED inline script blocks parse across ${#HTML_FILES[@]} files"
  fi
  rm -rf "$TMPD"
else
  say "skipped — node not found"
fi

# ── 2. Dictionary parity between pt and es ────────────────────────────────────
# A missing key is harmless at runtime (falls back to English) but shows an
# English sentence inside a Spanish screen. Escaped apostrophes inside keys
# ("your org\'s rate limit") are swapped for \x01 before parsing, then restored —
# without that the naive quote regex splits the key in the wrong place.
hdr "2. Dictionary parity (pt vs es)"
extract_keys() { # $1 = lang label
  awk -v want="$1" '
    /^  (pt|es):\{/ { cur = ($0 ~ /pt/) ? "pt" : "es"; next }
    /^  \},/        { cur = "" ; next }
    cur == want     { print }
  ' "$CONSOLE" \
  | sed "s/\\\\'/\x01/g" \
  | grep -oE "'[^']*':" \
  | sed "s/':$//; s/^'//; s/\x01/'/g" \
  | sort -u
}
PT_KEYS="$(extract_keys pt)"
ES_KEYS="$(extract_keys es)"
PT_N="$(printf '%s\n' "$PT_KEYS" | grep -c . || true)"
ES_N="$(printf '%s\n' "$ES_KEYS" | grep -c . || true)"
say "pt: $PT_N keys · es: $ES_N keys"
MISSING_ES="$(comm -23 <(printf '%s\n' "$PT_KEYS") <(printf '%s\n' "$ES_KEYS"))"
MISSING_PT="$(comm -13 <(printf '%s\n' "$PT_KEYS") <(printf '%s\n' "$ES_KEYS"))"
if [ -n "$MISSING_ES" ]; then
  say "FAIL — in pt but missing from es:"
  printf '%s\n' "$MISSING_ES" | sed 's/^/    /'
  FAIL=1
fi
if [ -n "$MISSING_PT" ]; then
  say "FAIL — in es but missing from pt:"
  printf '%s\n' "$MISSING_PT" | sed 's/^/    /'
  FAIL=1
fi
[ -z "$MISSING_ES$MISSING_PT" ] && say "ok — dictionaries match"

# ── 3. Untagged non-English user-facing text ──────────────────────────────────
# Heuristic: an accented character on a markup line that carries no data-i18n*
# attribute is text the selector cannot reach. Comments are excluded — those are
# internal and covered by check 4. Dictionary blocks are excluded — the accents
# there ARE the translations.
hdr "3. Untagged user-facing text (markup)"
UNTAGGED="$(awk '
  # Multi-line comment state. Without this, the middle lines of a /* … */ or
  # <!-- … --> block are reported as user-facing text — they are not, and the
  # false positives drown the real findings (356 vs 60 in practice).
  {
    line = $0
    if (inblock) {
      if (line ~ /\*\// || line ~ /-->/) inblock = 0
      next
    }
    # opens a block and does not close it on the same line
    if ((line ~ /\/\*/ && line !~ /\*\//) || (line ~ /<!--/ && line !~ /-->/)) { inblock = 1; next }
  }
  /^  (pt|es):\{/ { indict=1 } indict && /^  \},/ { indict=0; next }
  indict { next }
  {
    if ($0 ~ /data-i18n/) next
    # The language selector lists each language in ITS OWN language, by design —
    # "Português (Brasil)" must never become "Portuguese (Brazil)". Exempt it.
    if ($0 ~ /const LANGS=/) next
    # whole-line comment
    if ($0 ~ /^[[:space:]]*(\/\/|\/\*|\*|<!--)/) next
    # Strip TRAILING comments before testing. A CSS declaration such as
    #   --primary: 34 197 94;  /* brand — ação, sucesso */
    # is not user-facing text, and without this it dominates the report.
    # `//` is only stripped when the line has no scheme (://), so URLs survive.
    probe = $0
    gsub(/\/\*.*\*\//, "", probe)
    gsub(/<!--.*-->/, "", probe)
    if (probe !~ /:\/\//) sub(/\/\/.*$/, "", probe)
    if (probe ~ /[áàâãéêíóôõúüçÁÀÂÃÉÊÍÓÔÕÚÜÇ]/)
      printf "%6d  %s\n", NR, substr($0, 1, 110)
  }
' "$CONSOLE")"
UN_N="$(printf '%s\n' "$UNTAGGED" | grep -c . || true)"
if [ "$UN_N" -gt 0 ]; then
  say "$UN_N lines still carry non-English user-facing text (first 15):"
  printf '%s\n' "$UNTAGGED" | head -15
  FAIL=1
else
  say "ok — none found"
fi

# ── 3b. Tagged strings with no dictionary entry ───────────────────────────────
# A data-i18n whose source is absent from the dictionaries renders English even
# when the user picked pt/es. It fails silently — the selector simply appears to
# do nothing on that panel. Check 2 (parity) does not catch it, because a string
# missing from BOTH dictionaries is perfectly "in parity".
# HTML entities are decoded first: the attribute carries &amp;/&quot; but the DOM
# hands `dataset.i18n` the decoded text, which is what the key must match.
hdr "3b. Tagged strings missing from dictionaries"
TAGGED="$(grep -oE 'data-i18n(-ph|-al|-title)?="[^"]*"' "$CONSOLE" \
  | sed -E 's/^data-i18n(-ph|-al|-title)?="//; s/"$//' \
  | sed 's/&amp;/\&/g; s/&quot;/"/g; s/&lt;/</g; s/&gt;/>/g; s/&#39;/'"'"'/g' \
  | sort -u)"
NOKEY="$(comm -23 <(printf '%s\n' "$TAGGED") <(printf '%s\n' "$PT_KEYS"))"
NOKEY_N="$(printf '%s\n' "$NOKEY" | grep -c . || true)"
if [ "$NOKEY_N" -gt 0 ]; then
  say "FAIL — $NOKEY_N tagged strings have no pt/es entry (they will stay English):"
  printf '%s\n' "$NOKEY" | head -25 | sed 's/^/    /'
  [ "$NOKEY_N" -gt 25 ] && say "    … and $((NOKEY_N - 25)) more"
  FAIL=1
else
  say "ok — every tagged string is translated"
fi

# ── 3c. _t() literals with no dictionary entry ────────────────────────────────
# Same failure as 3b but for the JS side: `_t('English source')` renders English
# even when the user picked pt/es. Only single-quoted literals are matched —
# `_t(variable)` and template literals are out of reach for grep and are checked
# at the label-map definition instead.
hdr "3c. _t() literals missing from dictionaries"
TCALLS="$(sed "s/\\\\'/\x01/g" "$CONSOLE" \
  | grep -oE "_t\('[^']*'" \
  | sed "s/^_t('//; s/'$//; s/\x01/'/g" \
  | sort -u)"
TNOKEY="$(comm -23 <(printf '%s\n' "$TCALLS") <(printf '%s\n' "$PT_KEYS"))"
TNOKEY_N="$(printf '%s\n' "$TNOKEY" | grep -c . || true)"
if [ "$TNOKEY_N" -gt 0 ]; then
  say "FAIL — $TNOKEY_N _t() strings have no pt/es entry:"
  printf '%s\n' "$TNOKEY" | head -25 | sed 's/^/    /'
  [ "$TNOKEY_N" -gt 25 ] && say "    … and $((TNOKEY_N - 25)) more"
  FAIL=1
else
  say "ok — every _t() literal is translated"
fi

# ── 3d. Untranslated prose assigned straight to the DOM ───────────────────────
# The hole that let ~40 Portuguese strings survive every other gate: a JS literal
# written directly to the screen, with no accent to betray it.
#   check 3  only scans MARKUP text nodes, not JS.
#   check 3b only scans data-i18n attributes.
#   check 3c only scans what is already inside _t(...) — so a string that was never
#            wrapped is invisible to it by construction.
#
# Signal used: a quoted literal assigned to .textContent/.innerHTML, or passed to
# alert/confirm/prompt, that is NOT wrapped in _t() and looks like prose (a letter,
# then a space). Prose reaching the DOM unwrapped is the bug; a css class or an id
# has no space and is skipped.
hdr "3d. Prose written to the DOM without _t()"
# A line that calls _t() anywhere is already participating in translation; the bare
# literal next to it is the HTML wrapper, not the message. Requiring the ABSENCE of
# _t() on the line is what keeps this check quiet enough to be read — a noisy gate
# gets ignored, which is worse than no gate.
#
# Then: take the quoted literals, drop HTML tags and entities, and only report what
# still reads as prose (two consecutive words). A css class, an id, an option value
# or a single word survives the strip without matching.
RAWDOM="$(grep -nE "(\.(textContent|innerHTML) *= *|(alert|confirm|prompt)\()'" "$CONSOLE" \
  | grep -v '_t(' \
  | grep -v 'data-i18n' \
  | awk '{
      line = $0
      probe = ""
      n = split(line, q, /'"'"'/)
      for (i = 2; i <= n; i += 2) probe = probe " " q[i]
      gsub(/<[^>]*>/, " ", probe)     # HTML tags are structure, not message
      gsub(/&[a-z]+;/, " ", probe)    # entities likewise
      gsub(/[+]/, " ", probe)
      if (probe ~ /[A-Za-zÀ-ÿ][A-Za-zÀ-ÿ]+[ ][A-Za-zÀ-ÿ][A-Za-zÀ-ÿ]+/) print
    }' || true)"
RAW_N="$(printf '%s\n' "$RAWDOM" | grep -c . || true)"
if [ "$RAW_N" -gt 0 ]; then
  say "FAIL — $RAW_N literal(s) written to the DOM without _t() (first 12):"
  printf '%s\n' "$RAWDOM" | head -12 | cut -c1-140 | sed 's/^/    /'
  [ "$RAW_N" -gt 12 ] && say "    … and $((RAW_N - 12)) more"
  FAIL=1
else
  say "ok — no unwrapped prose reaches the DOM"
fi

# ── 4. Internal text still in Portuguese ──────────────────────────────────────
# Informational, not a gate: the backlog is 3k+ lines and is being converted in
# stages. Fails nothing, so a normal change is not blocked by legacy debt.
hdr "4. Internal text in Portuguese (informational)"
GO_N="$(grep -rhE '[áàâãéêíóôõúüçÁÉÍÓÚÃÕÇ]' --include='*.go' "$ROOT/domains" 2>/dev/null | grep -c . || true)"
CMT_N="$(grep -cE '^[[:space:]]*(\/\/|\/\*|<!--).*[áàâãéêíóôõúüç]' "$CONSOLE" || true)"
say "Go lines: $GO_N · console.html comments: $CMT_N"
say "(informational — see .kiro/specs/i18n-console/inventory.md)"

# ── 5. Help content coverage (domains/help) ───────────────────────────────────
# The console is not the only user-facing surface: the help drawer serves markdown
# from the help domain, one file per topic per language. A missing file fails
# SILENTLY — the fallback chain in manifest.json resolves to English, the drawer
# opens, the screen looks fine, and a Spanish user reads English. Same silent
# failure as 3b/3c, different surface, so it gets the same gate.
#
# Two things are checked: the body file exists and is non-empty for every declared
# language, and every internal (deep-dive) topic has a title in every language —
# the title is what the reader sees in the topic list before clicking.
hdr "5. Help content coverage (faq + internal × languages)"
HELP_CONTENT="$ROOT/domains/help/src/internal/adapters/embedstore/content"
MANIFEST="$HELP_CONTENT/manifest.json"
if [ ! -f "$MANIFEST" ]; then
  say "skipped — manifest not found at $MANIFEST"
else
  HELP_LANGS="$(grep -oE '"languages": *\[[^]]*\]' "$MANIFEST" \
    | grep -oE '"[a-z-]+"' | tr -d '"' | grep -v '^languages$')"
  say "languages: $(printf '%s' "$HELP_LANGS" | tr '\n' ' ')"

  # kind + file, section-aware. The 2-space `},` only closes a top-level section,
  # so the nested title blocks (6 spaces) do not end the walk.
  ENTRIES="$(awk '
    /^  "faq": *\{/      { kind="faq";      next }
    /^  "internal": *\{/ { kind="internal"; next }
    /^  \},?$/           { kind="";         next }
    kind != "" && match($0, /"file": *"[^"]*"/) {
      s = substr($0, RSTART, RLENGTH); sub(/.*"file": *"/, "", s); sub(/"$/, "", s)
      print kind "/" s
    }
  ' "$MANIFEST")"
  ENT_N="$(printf '%s\n' "$ENTRIES" | grep -c . || true)"

  MISSING=""
  for lang in $HELP_LANGS; do
    for e in $ENTRIES; do
      f="$HELP_CONTENT/${e%%/*}/$lang/${e#*/}"
      [ -s "$f" ] || MISSING="$MISSING
    ${e%%/*}/$lang/${e#*/}"
    done
  done

  # Deep-dive titles: one per internal topic per language.
  NOTITLE="$(awk -v langs="$(printf '%s' "$HELP_LANGS" | tr '\n' ' ')" '
    /^  "internal": *\{/ { inint=1; next }
    inint && /^  \},?$/  { inint=0 }
    inint && match($0, /^    "[a-z_]+": *\{/) {
      if (topic != "") check(topic)
      topic = $0; sub(/^    "/, "", topic); sub(/".*/, "", topic)
      delete seen
    }
    inint && match($0, /^        "[a-z-]+":/) {
      l = $0; sub(/^        "/, "", l); sub(/".*/, "", l); seen[l] = 1
    }
    END { if (topic != "") check(topic) }
    function check(t) {
      n = split(langs, a, " ")
      for (i = 1; i <= n; i++) if (!(a[i] in seen)) print "    title " t " [" a[i] "]"
    }
  ' "$MANIFEST")"

  if [ -n "$MISSING" ] || [ -n "$NOTITLE" ]; then
    say "FAIL — help content gaps (they fall back to English in silence):"
    [ -n "$MISSING" ] && printf '%s\n' "$MISSING" | grep .
    [ -n "$NOTITLE" ] && printf '%s\n' "$NOTITLE"
    FAIL=1
  else
    say "ok — $ENT_N topics present and titled in every language"
  fi
fi

# ── 6. Backend error messages vs the console dictionary ───────────────────────
# The Go domains answer {"error":"<English sentence>"} and the console renders it
# through _terr() → _t(). Because the English string IS the key, the Go literal and
# the dictionary key must match byte for byte. Nothing else catches a drift here:
# checks 3b/3c only see literals inside console.html, and these strings arrive at
# runtime. Rewording a message in Go without touching the dictionary silently
# reverts that one error to English.
#
# Only fixed messages are checked. A message built by concatenation cannot be a key
# and is expected to stay English — that is a known, accepted limitation.
hdr "6. Backend error messages missing from dictionaries"
GO_ERRS="$(grep -rhoE '"error": *"[^"+]*"' --include='*.go' "$ROOT/domains" 2>/dev/null \
  | sed -E 's/^"error": *"//; s/"$//' \
  | grep -vE '^(not found|unauthorized|forbidden|bad request|internal error)$' \
  | grep -vE ': ?$' \
  | sort -u)"
if [ -z "$GO_ERRS" ]; then
  say "skipped — no fixed backend error messages found"
else
  GO_N="$(printf '%s\n' "$GO_ERRS" | grep -c . || true)"
  # A message still carrying an accent is Portuguese that was never converted —
  # report it separately, because translating it is not the fix; rewriting the Go
  # literal in English is.
  GO_PT="$(printf '%s\n' "$GO_ERRS" | grep -E '[áàâãéêíóôõúüçÁÀÂÃÉÊÍÓÔÕÚÜÇ]' || true)"
  GO_UNTRANSLATED="$(comm -23 <(printf '%s\n' "$GO_ERRS") <(printf '%s\n' "$PT_KEYS") | grep -vE '[áàâãéêíóôõúüçÁÀÂÃÉÊÍÓÔÕÚÜÇ]' || true)"
  PT_N="$(printf '%s\n' "$GO_PT" | grep -c . || true)"
  UN_N2="$(printf '%s\n' "$GO_UNTRANSLATED" | grep -c . || true)"
  if [ "$PT_N" -gt 0 ]; then
    say "FAIL — $PT_N backend message(s) still in Portuguese (rewrite the Go literal in English):"
    printf '%s\n' "$GO_PT" | head -10 | sed 's/^/    /'
    FAIL=1
  fi
  if [ "$UN_N2" -gt 0 ]; then
    say "FAIL — $UN_N2 backend message(s) have no pt/es entry (they will stay English):"
    printf '%s\n' "$GO_UNTRANSLATED" | head -15 | sed 's/^/    /'
    [ "$UN_N2" -gt 15 ] && say "    … and $((UN_N2 - 15)) more"
    FAIL=1
  fi
  [ "$PT_N" -eq 0 ] && [ "$UN_N2" -eq 0 ] && say "ok — all $GO_N fixed backend messages are English and translated"
fi

say ""
if [ "$FAIL" -eq 0 ]; then say "PASS"; else say "FINDINGS — see above"; fi
exit "$FAIL"
