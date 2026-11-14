#!/usr/bin/env bash
# GOV-UX-004 / GOV-SEC-003: re-key member access records from the mutable e-mail to the
# immutable Cognito `sub`.
#
# Background: before GOV-SEC-003 the member access record was keyed MEMBER#<org>#<email>.
# It is now MEMBER#<org>#<sub>. The pre-token trigger reads ONLY the sub key, so on an
# existing deployment any e-mail-keyed record is invisible after the upgrade: a team-scoped
# member loses their team claim (usage-api 403), and a non-admin with no readable record is
# scope-denied (GOV-SEC-011). This script copies each e-mail-keyed record to its sub key and
# deletes the old one.
#
# SAFE: copy-first, verify the copy, then delete the old key. Idempotent — a record already
# keyed by sub is skipped, so a second run migrates 0. DRY-RUN by default: it only lists what
# it WOULD migrate; pass --apply to perform the writes.
#
# Usage:
#   scripts/migrate-members-to-sub.sh                 # dry run (default)
#   scripts/migrate-members-to-sub.sh --apply         # perform the migration
# Env (with poc defaults):
#   CONFIG_TABLE   (default aiplat-poc-gov-config)
#   USER_POOL_ID   (required — the Cognito pool, e.g. us-west-2_xxxxxxxxx)
#   AWS_REGION     (default us-west-2)
set -euo pipefail

APPLY=0
[ "${1:-}" = "--apply" ] && APPLY=1

TABLE="${CONFIG_TABLE:-aiplat-poc-gov-config}"
REGION="${AWS_REGION:-us-west-2}"
POOL="${USER_POOL_ID:-}"
if [ -z "$POOL" ]; then
  echo "USER_POOL_ID is required (the Cognito user pool id)." >&2
  exit 2
fi

echo "table=$TABLE region=$REGION pool=$POOL apply=$APPLY"

# All MEMBER# records (pk only).
PKS="$(aws dynamodb scan --table-name "$TABLE" --region "$REGION" \
  --filter-expression 'begins_with(pk, :p)' \
  --expression-attribute-values '{":p":{"S":"MEMBER#"}}' \
  --query 'Items[].pk.S' --output json)"

migrated=0 skipped=0 orphaned=0
# Iterate the pk strings.
for pk in $(echo "$PKS" | python3 -c 'import sys,json; [print(x) for x in json.load(sys.stdin)]'); do
  rest="${pk#MEMBER#}"          # <org>#<rest>
  org="${rest%%#*}"            # <org>
  key="${rest#*#}"             # <email-or-sub>
  if [[ "$key" != *"@"* ]]; then
    skipped=$((skipped+1)); continue        # already sub-keyed (a sub is a uuid, no '@')
  fi
  email="$key"
  sub="$(aws cognito-idp admin-get-user --user-pool-id "$POOL" --region "$REGION" \
          --username "$email" --query "UserAttributes[?Name=='sub'].Value | [0]" \
          --output text 2>/dev/null || true)"
  if [ -z "$sub" ] || [ "$sub" = "None" ]; then
    echo "ORPHAN  $pk  (no Cognito user for $email — left in place)"
    orphaned=$((orphaned+1)); continue
  fi
  newpk="MEMBER#${org}#${sub}"
  if [ "$APPLY" = "0" ]; then
    echo "WOULD MIGRATE  $pk  ->  $newpk"
    migrated=$((migrated+1)); continue
  fi
  item="$(aws dynamodb get-item --table-name "$TABLE" --region "$REGION" \
           --key "{\"pk\":{\"S\":\"$pk\"}}" --query 'Item' --output json)"
  cfg="$(echo "$item" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("config",{}).get("S",""))')"
  aws dynamodb put-item --table-name "$TABLE" --region "$REGION" \
    --item "{\"pk\":{\"S\":\"$newpk\"},\"config\":{\"S\":$(python3 -c 'import json,sys;print(json.dumps(sys.argv[1]))' "$cfg")}}"
  # Verify the copy before deleting the old key.
  got="$(aws dynamodb get-item --table-name "$TABLE" --region "$REGION" \
          --key "{\"pk\":{\"S\":\"$newpk\"}}" --query 'Item.config.S' --output text)"
  if [ "$got" != "$cfg" ]; then
    echo "FAIL verify for $newpk — leaving $pk intact" >&2; exit 3
  fi
  aws dynamodb delete-item --table-name "$TABLE" --region "$REGION" --key "{\"pk\":{\"S\":\"$pk\"}}"
  echo "MIGRATED  $pk  ->  $newpk"
  migrated=$((migrated+1))
done

echo "SUMMARY apply=$APPLY migrated_or_would=$migrated skipped_already_sub=$skipped orphaned=$orphaned"
[ "$APPLY" = "0" ] && echo "(dry run — re-run with --apply to perform the migration)"
