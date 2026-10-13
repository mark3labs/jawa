import { launchBrowser } from './browser.mjs';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

// Use the real exported Board/Archive pages and production embedded assets.
const fixture = process.env.JAWA_CARD_VISUAL_HTML;
assert.ok(fixture, 'Set JAWA_CARD_VISUAL_HTML to the TestCardVisualFixture export');
const assets = new URL('../assets/', import.meta.url);
const screenshots = process.env.JAWA_FIRST_PAINT_SCREENSHOT_DIR;
if (screenshots) await mkdir(screenshots, { recursive: true });
const server = createServer(async (req, res) => {
  const path = new URL(req.url, 'http://localhost').pathname;
  const asset = /^\/assets\/([a-zA-Z0-9_.-]+)$/.exec(path);
  const file = path === '/board' ? fixture : path === '/archive' ? `${fixture}.archive` :
    asset ? fileURLToPath(new URL(asset[1], assets)) : null;
  if (!file) { res.writeHead(404).end(); return; }
  try {
    const content = await readFile(file);
    const type = asset ? path.endsWith('.css') ? 'text/css' : path.endsWith('.js') ?
      'text/javascript' : 'application/octet-stream' : 'text/html';
    res.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-store' }).end(content);
  } catch { res.writeHead(404).end(); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const origin = `http://127.0.0.1:${server.address().port}`;

// Each resource has an independent gate, automatically bounded even on failures.
function gate() {
  let release;
  const promise = new Promise(resolve => { release = resolve; });
  let expired = false;
  const timer = setTimeout(() => { expired = true; release(); }, 8000);
  return { promise, get expired() { return expired; }, release: () => { clearTimeout(timer); release(); } };
}
async function geometry(page) {
  const boxes = await page.locator('svg.ic').evaluateAll(icons => icons.map(icon => {
    const { width, height } = icon.getBoundingClientRect();
    return { width, height };
  }));
  assert.ok(boxes.length > 0, 'shared icons must be present');
  assert.ok(boxes.every(b => b.width <= 16 && b.height <= 16), 'oversized shared icon');
  assert.ok(boxes.some(b => b.width > 0 && b.height > 0), 'icons must not all be hidden');
  for (const logo of await page.locator('svg.logo').all()) {
    const box = await logo.boundingBox();
    assert.equal(box.width, Number(await logo.getAttribute('width')));
    assert.equal(box.height, Number(await logo.getAttribute('height')));
  }
}
async function paint(page) {
  // A compositor screenshot forces a frame even in headless/JS-disabled contexts.
  await page.screenshot({ timeout: 5000 });
  await page.waitForFunction(() => performance.getEntriesByType('paint').some(e => e.name === 'first-contentful-paint'));
  assert.ok(await page.locator('.shell').isVisible(), 'content must not require a JS reveal');
}
async function styled(page) {
  await paint(page);
  await geometry(page);
  for (const [selector, size] of [['.nav-item > .ic', 16], ['.lane-head .lane-ic', 15], ['.archive-action .ic', 12], ['.icon-btn .ic', 15]]) {
    const icons = page.locator(selector);
    if (selector === '.lane-head .lane-ic' && new URL(page.url()).pathname === '/archive') continue;
    assert.ok(await icons.count() > 0, `missing contextual sample: ${selector}`);
    for (const box of await icons.evaluateAll(nodes => nodes.map(n => n.getBoundingClientRect().width))) {
      assert.equal(box, size, `${selector} must retain external CSS size`);
    }
  }
}
let browser;
try {
  browser = await launchBrowser();
  console.log(`Chromium ${browser.version()}`);
  for (const width of [390, 1440]) {
    for (const mode of ['failed-css', 'held-css', 'held-js', 'disabled-js']) {
      const context = await browser.newContext({ viewport: { width, height: 1000 },
        serviceWorkers: 'block', javaScriptEnabled: mode !== 'disabled-js' });
      const page = await context.newPage();
      page.setDefaultTimeout(10000);
      let css, js;
      let cssSeen = 0, jsSeen = 0;
      await context.route('**/assets/style.css', async route => {
        cssSeen++;
        if (mode === 'held-css' || mode === 'failed-css') await css.promise;
        if (mode === 'failed-css') await route.abort('failed');
        else await route.continue();
      });
      await context.route('**/assets/app.js', async route => {
        jsSeen++;
        await js.promise;
        await route.continue();
      });
      try {
        for (const action of ['cold', 'reload', 'link', 'back']) {
          css = gate(); js = gate();
          const expectedPath = action === 'link' ? '/archive' : '/board';
          const priorCSS = cssSeen, priorJS = jsSeen;
          // Wait only for commit, never for load/network-idle while a gate is held.
          if (action === 'cold') await page.goto(`${origin}/board`, { waitUntil: 'commit' });
          else if (action === 'reload') await page.reload({ waitUntil: 'commit' });
          else if (action === 'back') await page.goBack({ waitUntil: 'commit' });
          else {
            await Promise.all([
              page.waitForURL(`${origin}/archive`, { waitUntil: 'commit' }),
              page.locator('a.nav-item[href="/archive"]').click({ noWaitAfter: true }),
            ]);
          }
          assert.equal(new URL(page.url()).pathname, expectedPath);
          await page.locator('.shell').waitFor({ state: 'attached' });
          await page.waitForFunction(() => document.querySelector('svg.ic') !== null);
          assert.ok(cssSeen > priorCSS, `${action} must request CSS`);
          if (mode !== 'disabled-js') assert.ok(jsSeen > priorJS, `${action} must request JS`);
          if (mode !== 'failed-css') await geometry(page);
          assert.equal(css.expired || js.expired, false, 'asset gate expired before interval sample');
          const paints = await page.evaluate(() => performance.getEntriesByType('paint').map(e => e.name));
          // This is a DOM/computed-geometry sample, not evidence of paint with CSS held.
          console.log(`${width} ${mode} ${action}: interval paint entries ${JSON.stringify(paints)}`);
          css.release();
          if (mode === 'failed-css') {
            await paint(page);
            await geometry(page);
            const colors = await page.evaluate(() => [document.documentElement, document.body].map(el => {
              const style = getComputedStyle(el);
              return [style.backgroundColor, style.color];
            }));
            assert.deepEqual(colors, Array(2).fill(['rgb(19, 15, 11)', 'rgb(238, 226, 207)']));
            assert.equal(await page.locator('body').evaluate(el => getComputedStyle(el).margin), '0px');
          } else {
            await page.waitForFunction(() => [...document.styleSheets].some(sheet => sheet.href?.endsWith('/assets/style.css')));
            await styled(page);
          }
          if (screenshots && action === 'cold') {
            await page.screenshot({ path: `${screenshots}/${mode}-${width}.png`, fullPage: true });
          }
          assert.equal(js.expired, false, 'script gate expired before paint assertions');
          js.release();
          await page.waitForLoadState('load');
          if (mode !== 'failed-css') await styled(page);
        }
      } finally {
        css?.release(); js?.release();
        await context.close();
      }
      console.log(`${width} ${mode}: cold load, reload, native Board→Archive and back passed`);
    }
  }
} finally {
  if (browser) await browser.close();
  await new Promise(resolve => server.close(resolve));
}
