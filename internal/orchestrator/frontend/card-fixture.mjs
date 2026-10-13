import { chromium } from 'playwright';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

// Export with JAWA_CARD_VISUAL_HTML=/path/cards.html go test
// ./internal/orchestrator -run '^TestCardVisualFixture$' -count=1, then run
// JAWA_CARD_VISUAL_HTML=/path/cards.html npm run test:cards in this directory.
// Only deterministic fixtures and embedded assets are served; no live backend.
const fixture = process.env.JAWA_CARD_VISUAL_HTML;
assert.ok(fixture, 'Set JAWA_CARD_VISUAL_HTML to the exported fixture path');
const assets = new URL('../assets/', import.meta.url);
const server = createServer(async (req, res) => {
  const path = new URL(req.url, 'http://localhost').pathname;
  const asset = /^\/assets\/([a-zA-Z0-9_.-]+)$/.exec(path);
  const file = path === '/board' ? fixture : path === '/archive' ? `${fixture}.archive` :
    asset ? fileURLToPath(new URL(asset[1], assets)) : null;
  if (!file) { res.writeHead(404).end(); return; }
  try {
    const content = await readFile(file);
    const type = path === '/board' || path === '/archive' ? 'text/html' :
      path.endsWith('.css') ? 'text/css' : path.endsWith('.js') ? 'text/javascript' :
        path.endsWith('.svg') ? 'image/svg+xml' : 'application/octet-stream';
    res.writeHead(200, { 'Content-Type': type }).end(content);
  } catch { res.writeHead(404).end(); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser;
try {
  browser = await chromium.launch({ headless: true, args: ['--no-sandbox'],
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const page = await browser.newPage();
  page.setDefaultTimeout(10000);
  for (const width of [320, 390, 1440]) {
    await page.setViewportSize({ width, height: 1000 });
    for (const view of ['board', 'archive']) {
      await page.goto(`http://127.0.0.1:${server.address().port}/${view}`);
      await page.locator('.task-card').first().waitFor();
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `${view} ${width}: document overflow`);
      const overflow = await page.locator('.task-card').evaluateAll(cards =>
        cards.filter(c => c.scrollWidth > c.clientWidth + 1).map(c => c.id));
      assert.deepEqual(overflow, [], `${view} ${width}: card overflow`);
      assert.ok(await page.locator('[title="Assigned agent"]').count() > 0);
      if (view === 'board' && width === 1440) {
        const rows = await page.locator('.lane-actions').evaluateAll(rows => rows.map(r => r.getBoundingClientRect().bottom));
        assert.equal(rows.length, 5);
        assert.ok(Math.max(...rows) - Math.min(...rows) <= 1, 'Desktop lane action rows are misaligned');
      }
      for (const card of await page.locator('.task-card').all()) {
        const link = card.locator('.card-view-action');
        assert.equal(await link.count(), 1);
        const href = `/planning?card=${encodeURIComponent(await card.getAttribute('data-card-id'))}`;
        assert.equal(await link.getAttribute('href'), href);
        assert.equal(await link.getAttribute('aria-label'), 'View card and plan');
        assert.equal(await link.evaluate(el => getComputedStyle(el).opacity), '1');
        await card.locator('.archive-action').focus();
        await page.keyboard.press('Tab');
        assert.ok(await link.evaluate(el => document.activeElement === el));
        assert.ok(await link.evaluate(el => getComputedStyle(el).outlineStyle !== 'none' && parseFloat(getComputedStyle(el).outlineWidth) >= 2));
        if (width < 860) {
          const box = await link.boundingBox();
          assert.ok(box.width >= 44 && box.height >= 44, 'Shortcut touch target');
        }
      }
      if (process.env.JAWA_CARD_SCREENSHOT_DIR) {
        await page.screenshot({ path: `${process.env.JAWA_CARD_SCREENSHOT_DIR}/${view}-${width}.png`, fullPage: true });
      }
      // Intercept only the detail destination: the fixture has no backend detail route.
      for (const activation of ['click', 'Enter']) {
        await page.goto(`http://127.0.0.1:${server.address().port}/${view}`);
        const link = page.locator('.card-view-action').first();
        const href = await link.getAttribute('href');
        const requests = [];
        const track = req => requests.push({ method: req.method(), path: new URL(req.url()).pathname });
        page.on('request', track);
        await page.route('**/planning?card=*', route => route.fulfill({ contentType: 'text/html', body: '<h1>Synthetic detail destination</h1>' }));
        const navigation = page.waitForURL(`**${href}`);
        if (activation === 'click') await link.click();
        else { await link.focus(); await page.keyboard.press('Enter'); }
        await navigation;
        page.off('request', track);
        assert.deepEqual(requests, [{ method: 'GET', path: '/planning' }], 'Activation must navigate only, never archive or drag');
        await page.unroute('**/planning?card=*');
      }
      console.log(`${view} ${width}: overflow, assignment, shortcut, focus and native activation passed`);
    }
  }
} finally {
  if (browser) await browser.close();
  await new Promise(resolve => server.close(resolve));
}
