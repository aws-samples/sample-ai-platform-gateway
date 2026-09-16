// Role-gating check for the AIPlat console, against the local demo server.
//
//   node demo/server.mjs &                 # terminal 1
//   node demo/check-roles.mjs              # terminal 2
//
// Regression check for the F-03 audit finding: applyRoleNav() only hid the sidebar, so a
// dev/billing user reaching a gated panel through an Overview card (or any other non-nav
// path) landed on a fully interactive Save-everything screen. show(v) must now bounce back
// to 'overview' for a destination ROLE_VIEWS[auth.role] does not list.
//
// Requires playwright (already a devDependency of demo/):
//   npm i -D playwright && npx playwright install chromium
// or reuse an installed Google Chrome:
//   BROWSER_CHANNEL=chrome node demo/check-roles.mjs
//
// Session is faked the same way capture.mjs does: an unsigned JWT seeded into
// localStorage before any page script runs. Every API here is the local fixture server.

import { chromium } from 'playwright';

const BASE = process.env.DEMO_URL || 'http://127.0.0.1:8787';
const CHANNEL = process.env.BROWSER_CHANNEL || undefined;

const b64url = (o) =>
  Buffer.from(JSON.stringify(o)).toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');

function claimsFor(role) {
  return {
    'custom:org_id': 'acme',
    'custom:role': role,
    team: role === 'dev' ? 'growth' : 'platform',
    email: `check-${role}@acme.example`,
    exp: Math.floor(Date.now() / 1000) + 86400,
  };
}

async function session(browser, role) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const claims = claimsFor(role);
  await ctx.addInitScript(
    ([token]) => {
      localStorage.setItem('aiplat_jwt', token);
      localStorage.setItem('aiplat_theme', 'dark');
      localStorage.setItem('aiplat_lang', 'en');
      // aiplat_at deliberately unset: the Settings 2FA card would call real Cognito.
    },
    [`${b64url({ alg: 'none', typ: 'JWT' })}.${b64url(claims)}.check`],
  );
  const page = await ctx.newPage();
  await page.goto(`${BASE}/console.html`, { waitUntil: 'networkidle' });
  await page.waitForSelector('#login', { state: 'hidden', timeout: 15000 });
  return { ctx, page };
}

async function assertGated(page, view, label) {
  await page.evaluate((v) => window.show(v), view);
  await page.waitForTimeout(200);
  const state = await page.evaluate((v) => ({
    hidden: document.querySelector(`[data-panel="${v}"]`)?.classList.contains('hidden'),
    title: document.querySelector('#viewTitle')?.textContent,
  }), view);
  if (state.hidden !== true || state.title !== 'Overview') {
    throw new Error(`${label}: show('${view}') should bounce to Overview — got hidden=${state.hidden} title=${JSON.stringify(state.title)}`);
  }
}

async function assertOpens(page, view, label) {
  await page.evaluate((v) => window.show(v), view);
  await page.waitForTimeout(200);
  const hidden = await page.evaluate((v) => document.querySelector(`[data-panel="${v}"]`)?.classList.contains('hidden'), view);
  if (hidden !== false) throw new Error(`${label}: show('${view}') should have opened the panel, it stayed hidden`);
}

async function main() {
  const browser = await chromium.launch({ channel: CHANNEL });
  const failures = [];
  try {
    {
      const { ctx, page } = await session(browser, 'dev');
      try {
        await assertGated(page, 'limits', 'dev');
        await assertGated(page, 'guardrails', 'dev');
        await assertGated(page, 'alerts', 'dev');
        await assertGated(page, 'teams', 'dev');
        await assertOpens(page, 'keys', 'dev'); // dev IS allowed here
      } catch (e) { failures.push(String(e.message || e)); }
      finally { await ctx.close(); }
    }
    {
      const { ctx, page } = await session(browser, 'billing');
      try {
        await assertGated(page, 'keys', 'billing');
        await assertGated(page, 'limits', 'billing');
        await assertOpens(page, 'usage', 'billing'); // billing IS allowed here
      } catch (e) { failures.push(String(e.message || e)); }
      finally { await ctx.close(); }
    }
    {
      const { ctx, page } = await session(browser, 'owner');
      try {
        await assertOpens(page, 'limits', 'owner');
        await assertOpens(page, 'guardrails', 'owner');
        await assertOpens(page, 'teams', 'owner');
      } catch (e) { failures.push(String(e.message || e)); }
      finally { await ctx.close(); }
    }
  } finally {
    await browser.close();
  }

  if (failures.length) {
    process.stdout.write(`${failures.length} failure(s):\n`);
    for (const f of failures) process.stdout.write(`  - ${f}\n`);
    process.exitCode = 1;
    return;
  }
  process.stdout.write('all role-gating checks passed\n');
}

main().catch((e) => {
  process.stderr.write(String(e && e.stack ? e.stack : e) + '\n');
  process.exit(1);
});
