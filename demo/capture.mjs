// Screenshot capture for the AIPlat console, against the local demo server.
//
//   node demo/server.mjs &                 # terminal 1
//   node demo/capture.mjs                  # terminal 2
//
// Writes assets/screenshots/*.png. Requires playwright:
//   npm i -D playwright && npx playwright install chromium
// or reuse an installed Google Chrome:
//   BROWSER_CHANNEL=chrome node demo/capture.mjs
//
// The session is faked by pre-seeding localStorage with an unsigned JWT before
// any page script runs: the console only base64-decodes the claims, so Cognito
// is never contacted. That is a demo shortcut, not a gateway bypass — every API
// here is the local fixture server.

import { mkdir } from 'node:fs/promises';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright';

const HERE = fileURLToPath(new URL('.', import.meta.url));
const OUT = join(HERE, '..', 'assets', 'screenshots');
const BASE = process.env.DEMO_URL || 'http://127.0.0.1:8787';
const CHANNEL = process.env.BROWSER_CHANNEL || undefined;

const b64url = (o) =>
  Buffer.from(JSON.stringify(o)).toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');

const CLAIMS = {
  'custom:org_id': 'acme',
  'custom:role': 'owner',
  team: 'platform',
  email: 'dana@acme.example',
  exp: Math.floor(Date.now() / 1000) + 86400,
};

// name    — output file
// view    — the console view to open
// wait    — selector proving the view finished rendering
// act     — extra interaction before the shot
// full    — capture the whole scrollable page instead of the viewport
// h       — viewport height for this shot; short views get a tighter frame
//           instead of a screenshot that is half empty background
const SHOTS = [
  { name: 'overview', view: 'overview', wait: '#ovCards [data-hc]', h: 660 },
  { name: 'usage', view: 'usage', wait: '#usageRows tr', full: true },
  { name: 'roi', view: 'roi', wait: '#roiRows tr', full: true },
  { name: 'models', view: 'models', wait: '#modelList .rounded-xl', full: true },
  { name: 'logs', view: 'logs', wait: '#lgRows tr' },
  {
    // No top-level `wait`: #avRows only exists after the sub-tab is clicked,
    // so this shot does its own waiting inside act().
    name: 'audit-trail', view: 'logs',
    act: async (page) => {
      await page.click('[data-ltab-btn="config"]');
      await page.waitForSelector('#avRows tr');
      // Expand the first diff so the field-level before/after is visible.
      const row = page.locator('[data-avtoggle]').first();
      if (await row.count()) await row.click();
    },
  },
  { name: 'guardrails', view: 'guardrails', wait: '#grList [data-gr]', h: 700 },
  { name: 'limits', view: 'limits', wait: '#lModels [data-lm-model]', h: 860 },
  { name: 'teams', view: 'teams', wait: '#tGrid .rounded-xl', full: true },
  { name: 'keys', view: 'keys', wait: '#kRows tr', h: 800 },
  { name: 'alerts', view: 'alerts', wait: '#alList [data-al]', full: true },
  { name: 'settings', view: 'settings', wait: '#crList table', full: true },
  {
    name: 'playground', view: 'play', h: 720,
    act: async (page) => {
      await page.fill('#pInput', 'Why put a gateway in front of the model providers?');
      await page.click('#pSend');
      await page.waitForSelector('#pChat .fade:nth-child(2)');
      await page.waitForTimeout(400);
    },
  },
];

async function main() {
  await mkdir(OUT, { recursive: true });

  const browser = await chromium.launch({ channel: CHANNEL });
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 940 },
    // deviceScaleFactor 1 on purpose: 1440px is already wider than GitHub's
    // README column, and 2x turned a full-page shot into a ~4 MB blob.
    deviceScaleFactor: 1,
    colorScheme: 'dark',
  });

  await ctx.addInitScript(
    ([token, claims]) => {
      localStorage.setItem('aiplat_jwt', token);
      localStorage.setItem('aiplat_gwkey', 'sk-aiplat-demo-key');
      localStorage.setItem('aiplat_theme', 'dark');
      localStorage.setItem('aiplat_lang', 'en');
      void claims;
    },
    [`${b64url({ alg: 'none', typ: 'JWT' })}.${b64url(CLAIMS)}.demo`, CLAIMS],
  );

  const page = await ctx.newPage();
  const failures = [];
  // The browser always asks for /favicon.ico and the console never ships one;
  // that single 404 is expected and must not fail the run.
  page.on('console', (m) => {
    if (m.type() === 'error' && !m.location().url.endsWith('/favicon.ico')) failures.push(m.text());
  });
  page.on('pageerror', (e) => failures.push(String(e)));

  await page.goto(`${BASE}/console.html`, { waitUntil: 'networkidle' });
  // boot() hides the login overlay once the seeded token is accepted. Waiting
  // for state:'hidden' (not the default 'visible') is the whole point here.
  await page.waitForSelector('#login', { state: 'hidden', timeout: 15000 });

  const WIDTH = 1440;
  for (const shot of SHOTS) {
    await page.setViewportSize({ width: WIDTH, height: shot.h || 940 });
    await page.evaluate((v) => window.show(v), shot.view);
    if (shot.wait) await page.waitForSelector(shot.wait, { timeout: 15000 });
    if (shot.act) await shot.act(page);
    // Chart.js animates on create; settle before capturing.
    await page.waitForTimeout(900);

    // fullPage does nothing here: the shell is `h-screen overflow-hidden` and
    // each panel scrolls internally, so the document never grows past the
    // viewport. To show a long panel in one image, grow the viewport to the
    // panel's own scrollHeight and take a normal viewport shot.
    let grew = 0;
    if (shot.full) {
      const need = await page.evaluate((v) => {
        const p = document.querySelector(`[data-panel="${v}"]`);
        const head = document.querySelector('header');
        return (p ? p.scrollHeight : 0) + (head ? head.offsetHeight : 0);
      }, shot.view);
      grew = Math.min(Math.max(need + 8, 940), 2600);
      await page.setViewportSize({ width: WIDTH, height: grew });
      // Charts re-layout on resize; give them time before the shutter.
      await page.waitForTimeout(800);
    }

    const path = join(OUT, `${shot.name}.png`);
    await page.screenshot({ path });
    process.stdout.write(`captured ${shot.name}.png${grew ? ` (${WIDTH}x${grew}, grown to fit)` : ''}\n`);
  }

  await browser.close();

  if (failures.length) {
    process.stdout.write(`\n${failures.length} console error(s) during capture:\n`);
    for (const f of [...new Set(failures)]) process.stdout.write(`  ${f}\n`);
    process.exitCode = 1;
    return;
  }
  process.stdout.write('\nno console errors\n');
}

main().catch((e) => {
  process.stderr.write(String(e && e.stack ? e.stack : e) + '\n');
  process.exit(1);
});
