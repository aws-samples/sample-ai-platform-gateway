// AgentCore Gateway registration check for the AIPlat console, against the local demo server.
//
//   node demo/server.mjs &                     # terminal 1
//   node demo/check-agentcore.mjs              # terminal 2
//   BROWSER_CHANNEL=chrome node demo/check-agentcore.mjs   # reuse installed Chrome
//
// The console has no unit-test harness, and the registration form is the only place a
// `bedrock_gateway` route gets built by a human. Three of its rules are things the SERVICE
// enforces and would otherwise surface as a confusing failure much later:
//
//   1. a model id containing ':' is rejected by the gateway outright
//   2. a signing region that disagrees with the endpoint is rejected as bad credentials
//   3. the extended-thinking shape is per-model and mutually exclusive — the wrong one makes
//      every reasoning request fail while plain requests keep working
//
// It asserts on what the console SAVES, not on its internals: `CFG` is module-scoped and not
// reachable from the page, and the persisted body is the real contract anyway — a route that
// looks right in memory and is written wrong is the bug that matters.
//
// Session is faked exactly as capture.mjs and check-roles.mjs do it.

import { chromium } from 'playwright';

const BASE = process.env.DEMO_URL || 'http://127.0.0.1:8787';
const CHANNEL = process.env.BROWSER_CHANNEL || undefined;
const GW = 'https://gw-abc123.gateway.bedrock-agentcore.us-east-1.amazonaws.com';

const b64url = (o) =>
  Buffer.from(JSON.stringify(o)).toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');

const CLAIMS = {
  'custom:org_id': 'acme',
  'custom:role': 'owner',
  team: 'platform',
  email: 'check-agentcore@acme.example',
  exp: Math.floor(Date.now() / 1000) + 86400,
};

// saved captures the body of the next PUT /admin/config the page makes.
let saved = null;

async function open(browser) {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 940 } });
  await ctx.addInitScript(
    ([token]) => {
      localStorage.setItem('aiplat_jwt', token);
      localStorage.setItem('aiplat_theme', 'dark');
      localStorage.setItem('aiplat_lang', 'en');
      // aiplat_at deliberately unset: the Settings 2FA card would call real Cognito.
    },
    [`${b64url({ alg: 'none', typ: 'JWT' })}.${b64url(CLAIMS)}.check`],
  );
  const page = await ctx.newPage();
  // Watch the save instead of stubbing it: the demo server answers /admin/config, so the
  // round trip stays real and the console's own reload-after-save still happens.
  page.on('request', (req) => {
    if (req.method() === 'PUT' && req.url().includes('/admin/config')) {
      try { saved = JSON.parse(req.postData() || '{}'); } catch { saved = null; }
    }
  });
  await page.goto(`${BASE}/console.html`, { waitUntil: 'networkidle' });
  await page.waitForSelector('#login', { state: 'hidden', timeout: 15000 });
  await page.evaluate(() => window.show('models'));
  await page.waitForTimeout(300);
  return { ctx, page };
}

async function selectProvider(page) {
  return page.evaluate(() => {
    const sel = document.querySelector('#provPick');
    if (!sel) return { ok: false, why: 'no #provPick' };
    const opt = [...sel.options].find((o) => o.value === 'agentcore');
    if (!opt) return { ok: false, why: 'no agentcore option in #provPick' };
    sel.value = 'agentcore';
    sel.dispatchEvent(new Event('change'));
    const form = document.querySelector('#addAgentcore');
    return { ok: !!form && !form.classList.contains('hidden'), why: 'form stayed hidden', label: opt.textContent };
  });
}

// add fills the form and clicks Add. Returns the message, the auto-selected thinking shape,
// and whether a row for the alias reached the route list.
async function add(page, { alias, model, base, region, think }) {
  return page.evaluate(({ alias, model, base, region, think }) => {
    const set = (id, v) => {
      const el = document.querySelector('#' + id);
      el.value = v;
      el.dispatchEvent(new Event('input'));
    };
    set('agAlias', alias);
    set('agBase', base);
    document.querySelector('#agRegion').value = region;
    set('agModel', model); // fires oninput, which auto-selects the thinking shape
    const autoThink = document.querySelector('#agThink').value;
    if (think) document.querySelector('#agThink').value = think;
    document.querySelector('#agAdd').click();
    const msgEl = document.querySelector('#agMsg');
    return {
      autoThink,
      msg: msgEl.textContent,
      msgClass: msgEl.className,
      listed: !!document.querySelector(`[data-m="${alias}"]`),
    };
  }, { alias, model, base, region, think });
}

// save clicks the models Save button and returns the routing/pricing actually sent.
async function save(page) {
  saved = null;
  await page.evaluate(() => document.querySelector('#mSave').click());
  for (let i = 0; i < 60 && saved === null; i++) await page.waitForTimeout(100);
  return saved;
}

function check(label, cond, detail) {
  if (!cond) throw new Error(`${label}${detail ? ': ' + detail : ''}`);
}

async function main() {
  const browser = await chromium.launch({ channel: CHANNEL });
  const failures = [];
  const { ctx, page } = await open(browser);
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));

  try {
    try {
      const r = await selectProvider(page);
      check('provider option/form', r.ok, r.why);
      check('option label names AgentCore Gateway', /AgentCore Gateway/.test(r.label || ''), r.label);
    } catch (e) { failures.push(String(e.message || e)); }

    // --- guard 1: a model id containing ':' must be refused BEFORE it is registered ---
    try {
      const r = await add(page, {
        alias: 'chk-colon', model: 'runtime/us.anthropic.claude-sonnet-4-5-20250929-v1:0',
        base: GW, region: 'us-east-1',
      });
      check('versioned id refused', r.listed === false, 'the route reached the list anyway');
      check('the message explains why', /":"/.test(r.msg), r.msg);
      check('the message is a warning', /amber/.test(r.msgClass), r.msgClass);
    } catch (e) { failures.push(String(e.message || e)); }

    // --- guard 2: a region that disagrees with the endpoint must be refused ---
    try {
      const r = await add(page, {
        alias: 'chk-region', model: 'mantle/anthropic.claude-haiku-4-5',
        base: GW, region: 'us-west-2',
      });
      check('region mismatch refused', r.listed === false, 'the route reached the list anyway');
      check('the message names the region', /region/i.test(r.msg), r.msg);
    } catch (e) { failures.push(String(e.message || e)); }

    // --- guard 3: an incomplete form must be refused ---
    try {
      const r = await add(page, { alias: 'chk-empty', model: '', base: '', region: 'us-east-1' });
      check('empty form refused', r.listed === false, 'the route reached the list anyway');
    } catch (e) { failures.push(String(e.message || e)); }

    // --- three valid routes, covering both thinking shapes ---
    //
    // chk-haiku gets TOGGLED below and chk-haiku2 does not, so a single save can prove both
    // the toggle and the auto-selected default. There is exactly ONE save on purpose: the
    // demo server does not persist a PUT, so after saveConfig's reload the test routes are
    // gone from the effective config and any further interaction silently no-ops — which
    // looks like a broken toggle and is really a broken test. Learned the hard way.
    try {
      const h = await add(page, {
        alias: 'chk-haiku', model: 'mantle/anthropic.claude-haiku-4-5',
        base: GW, region: 'us-east-1',
      });
      check('haiku listed', h.listed === true, h.msg);
      // Haiku 4.5 rejects the adaptive shape, so the form must not preselect it.
      check('thinking auto-selected budget for haiku', h.autoThink === 'budget', h.autoThink);

      const h2 = await add(page, {
        alias: 'chk-haiku2', model: 'mantle/anthropic.claude-haiku-4-5',
        base: GW, region: 'us-east-1',
      });
      check('second haiku listed', h2.listed === true, h2.msg);

      const o = await add(page, {
        alias: 'chk-opus', model: 'runtime/us.anthropic.claude-opus-4-8',
        base: GW, region: 'us-east-1',
      });
      check('opus listed', o.listed === true, o.msg);
      check('thinking auto-selected adaptive for opus', o.autoThink === 'adaptive', o.autoThink);
    } catch (e) { failures.push(String(e.message || e)); }

    // --- the per-route thinking toggle is rendered and flips (before any save) ---
    try {
      const r = await page.evaluate(() => {
        const btn = document.querySelector('[data-rs="chk-haiku"]');
        if (!btn) return { ok: false, why: 'no [data-rs] toggle rendered for a bedrock_gateway route' };
        const before = btn.textContent.trim();
        btn.click();
        const after = (document.querySelector('[data-rs="chk-haiku"]') || {}).textContent || '';
        return { ok: true, before, after: after.trim() };
      });
      check('thinking toggle rendered', r.ok, r.why);
      check('toggle label flips budget -> adaptive',
        /budget/.test(r.before) && /adaptive/.test(r.after), `${r.before} -> ${r.after}`);
    } catch (e) { failures.push(String(e.message || e)); }

    // --- the toggle must NOT appear where the adapter would ignore it ---
    //
    // Every fixture route is `bedrock` or `openai_compatible`, so the only toggles on the
    // page must be the three added above. A toggle on a `bedrock` route would be inert: that
    // adapter sends the budget shape through additionalModelRequestFields and has no
    // adaptive path.
    try {
      const present = await page.evaluate(() =>
        [...new Set([...document.querySelectorAll('[data-rs]')].map((b) => b.dataset.rs))].sort());
      // claude-direct is the fixture's native Anthropic route: same dialect, so it MUST have
      // the toggle. Everything else in the fixture set is bedrock or openai_compatible.
      check('toggle only on Anthropic-dialect routes',
        present.join(',') === 'chk-haiku,chk-haiku2,chk-opus,claude-direct', present.join(','));
    } catch (e) { failures.push(String(e.message || e)); }

    // --- the toggle on a SERVER-LOADED route: the case that actually breaks ---
    //
    // For a route that came from the add form, CFG and CFG_RAW hold the SAME object, so
    // mutating one updates both by accident. For a route loaded from the server they are two
    // separate fetches and therefore two different objects — so a handler that writes only
    // the effective config flips on screen and saves the old value. That is the whole reason
    // the raw write exists, and claude-direct is the only route here that can prove it.
    try {
      const r = await page.evaluate(() => {
        const btn = document.querySelector('[data-rs="claude-direct"]');
        if (!btn) return { ok: false, why: 'no toggle on the fixture anthropic route' };
        const before = btn.textContent.trim();
        btn.click();
        const after = (document.querySelector('[data-rs="claude-direct"]') || {}).textContent || '';
        return { ok: true, before, after: after.trim() };
      });
      check('toggle rendered on the server-loaded route', r.ok, r.why);
      // Absent reasoning_style means budget, so the first click must move it to adaptive.
      check('an unset shape reads as budget and flips to adaptive',
        /budget/.test(r.before) && /adaptive/.test(r.after), `${r.before} -> ${r.after}`);
    } catch (e) { failures.push(String(e.message || e)); }

    // --- what actually gets PERSISTED (the only save) ---
    try {
      const body = await save(page);
      check('a config PUT was captured', !!body && !!body.routing, 'no PUT body seen');
      const routing = body.routing || {};
      check('refused routes are absent from the saved config',
        !routing['chk-colon'] && !routing['chk-region'] && !routing['chk-empty'],
        Object.keys(routing).join(','));

      const h = routing['chk-haiku'];
      check('haiku route persisted', !!h, Object.keys(routing).join(','));
      check('provider is bedrock_gateway', h.provider === 'bedrock_gateway', h.provider);
      check('provider_model_id stays target-qualified',
        h.provider_model_id === 'mantle/anthropic.claude-haiku-4-5', h.provider_model_id);
      check('base_url persisted', h.base_url === GW, h.base_url);
      check('region persisted', h.region === 'us-east-1', h.region);
      check('prompt_cache on by default', h.prompt_cache === true, String(h.prompt_cache));
      check('reasoning capability declared', h.capabilities && h.capabilities.reasoning === true,
        JSON.stringify(h.capabilities));
      check('context window declared', h.capabilities.context_window_tokens === 200000,
        String(h.capabilities.context_window_tokens));
      // The toggle writes into the effective config; Save reads the RAW one. A flip that
      // shows on screen and saves the old value is the failure this asserts against.
      check('the toggled route saved adaptive', h.reasoning_style === 'adaptive', h.reasoning_style);

      const h2 = routing['chk-haiku2'];
      check('untouched haiku route persisted', !!h2, 'missing');
      check('the untouched route kept the auto-selected budget', h2.reasoning_style === 'budget',
        h2.reasoning_style);

      const o = routing['chk-opus'];
      check('opus route persisted', !!o, 'missing');
      check('reasoning_style adaptive for opus', o.reasoning_style === 'adaptive', o.reasoning_style);

      check('pricing entry created for the alias', !!(body.pricing && body.pricing['chk-haiku']),
        JSON.stringify(body.pricing && Object.keys(body.pricing)));

      // The assertion the earlier version of this check was missing entirely: the flip on a
      // SERVER-LOADED route has to reach the raw config, or Save writes the old value back.
      const d = routing['claude-direct'];
      check('the server-loaded route survived the save', !!d, Object.keys(routing).join(','));
      check('its flipped shape reached the raw config', d.reasoning_style === 'adaptive',
        String(d.reasoning_style));
    } catch (e) { failures.push(String(e.message || e)); }

    if (errors.length) failures.push(`page errors: ${errors.join(' | ')}`);
  } finally {
    await ctx.close();
    await browser.close();
  }

  if (failures.length) {
    process.stdout.write(`${failures.length} failure(s):\n`);
    for (const f of failures) process.stdout.write(`  - ${f}\n`);
    process.exitCode = 1;
    return;
  }
  process.stdout.write('all AgentCore Gateway registration checks passed\n');
}

main().catch((e) => {
  process.stderr.write(String(e && e.stack ? e.stack : e) + '\n');
  process.exit(1);
});
