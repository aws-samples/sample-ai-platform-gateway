// Shape check for the AIPlat demo fixtures, no deps, no network.
//
//   node demo/check-fixtures.mjs
//
// Regression check for the F-05 audit finding: the offline fixtures diverged from the
// real API response shapes, so console code paths keyed on real-only fields (status on
// keys, by_upstream, served_model_id, requested_cost_usd, cache_hit, status on records)
// were never exercised locally and were invisible in screenshots. This does not compare
// against the Go structs directly (that is Deferred D-3) — it pins the specific fields
// this fix added, so a future edit cannot drop them silently.

import * as fx from './fixtures.mjs';

let failures = 0;
function check(cond, label) {
  if (!cond) {
    failures++;
    process.stdout.write(`FAIL  ${label}\n`);
  } else {
    process.stdout.write(`ok    ${label}\n`);
  }
}

check(fx.KEYS.every((k) => typeof k.status === 'string' && k.status), 'every KEYS row has a status');
check(fx.KEYS.every((k) => typeof k.org === 'string' && k.org), 'every KEYS row has an org');
check(fx.KEYS.some((k) => k.status === 'revoked'), 'at least one KEYS row is revoked');

check(Array.isArray(fx.USAGE.by_upstream) && fx.USAGE.by_upstream.length > 0, 'USAGE has by_upstream');
check(typeof fx.USAGE.bucket === 'string', 'USAGE has bucket');
check(typeof fx.USAGE.from === 'string' && typeof fx.USAGE.to === 'string', 'USAGE has from/to');

const PROVIDER_FIELDS = [
  'credit_usd', 'cash_usd', 'saved_verified_usd', 'saved_counterfactual_usd',
  'cost_list_price_usd', 'cost_contract_price_usd',
];
check(
  fx.USAGE.by_provider.every((r) => PROVIDER_FIELDS.every((f) => typeof r[f] === 'number')),
  'every USAGE.by_provider row has ' + PROVIDER_FIELDS.join(', '),
);

const RECORD_FIELDS = ['requested_cost_usd', 'served_model_id', 'status', 'cache_hit'];
check(
  fx.RECORDS.length > 0 && fx.RECORDS.every((r) => RECORD_FIELDS.every((f) => f in r)),
  'every RECORDS row has ' + RECORD_FIELDS.join(', '),
);
check(fx.RECORDS.some((r) => r.cache_hit === true), 'at least one RECORDS row has cache_hit=true');
check(fx.RECORDS.some((r) => r.status === 'error'), 'at least one RECORDS row has status=error');
check(fx.RECORDS.some((r) => r.status === 'blocked'), 'at least one RECORDS row has status=blocked');

process.stdout.write(failures ? `\n${failures} failure(s)\n` : '\nall fixture shape checks passed\n');
process.exitCode = failures ? 1 : 0;
