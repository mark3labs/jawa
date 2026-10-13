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
  // A synthetic destination verifies native navigation, not backend rendering.
  if (path === '/planning' && req.method === 'GET') {
    res.writeHead(200, { 'Content-Type': 'text/html' }).end('<h1>Detail navigation fixture</h1>');
    return;
  }
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
      for (const [id, total, breakdown] of [
        ['abc', 4, '2 planning, 1 building, 1 fixing'],
        ['plan', 1, '1 planning, 0 building, 0 fixing'],
        ['fix', 1, '0 planning, 0 building, 1 fixing'],
        ['archive', 1, '0 planning, 1 building, 0 fixing'],
      ]) {
        const badge = page.locator(`[data-project-id="${id}"] .nav-count.live`);
        assert.equal(await badge.textContent(), String(total));
        const label = await badge.getAttribute('aria-label');
        assert.ok(label.includes(breakdown));
        assert.ok(label.includes('includes queued and archived work'));
        assert.equal(await badge.getAttribute('title'), label);
      }
      assert.equal(await page.locator('[data-project-id="zero"] .nav-count.live').count(), 0);
      if (view === 'board') {
        assert.match(await page.locator('.view-stats').textContent(), /4 active/);
      }
      const overlaps = await page.locator('[data-project-id]').evaluateAll(links =>
        links.filter(link => {
          const text = link.querySelector('.nav-text')?.getBoundingClientRect();
          const badge = link.querySelector('.nav-count')?.getBoundingClientRect();
          return text && badge && text.right > badge.left + 1;
        }).map(link => link.dataset.projectId));
      assert.deepEqual(overlaps, [], `${view} ${width}: project name overlaps badge`);
      const overflow = await page.locator('.task-card').evaluateAll(cards =>
        cards.filter(c => c.scrollWidth > c.clientWidth + 1).map(c => c.id));
      assert.deepEqual(overflow, [], `${view} ${width}: card overflow`);
      assert.ok(await page.locator('[title="Assigned agent"]').count() > 0);
      if (view === 'board' && width === 1440) {
        const rows = await page.locator('.lane-actions').evaluateAll(rows => rows.map(r => r.getBoundingClientRect().bottom));
        assert.equal(rows.length, 5);
        assert.ok(Math.max(...rows) - Math.min(...rows) <= 1, 'Desktop lane action rows are misaligned');
      }
      const cards = page.locator('.task-card');
      for (const card of await cards.all()) {
        const link = card.locator('.card-view-action');
        assert.equal(await link.count(), 1);
        const id = await card.getAttribute('data-card-id');
        assert.equal(await link.getAttribute('href'), `/planning?card=${encodeURIComponent(id)}`);
        assert.equal(await link.getAttribute('aria-label'), `View card and plan: ${await card.locator('.card-key').textContent()}`);
        assert.equal(await link.evaluate(el => getComputedStyle(el).opacity), '1');
        assert.equal(await link.evaluate(el => el.closest('form')), null);
        if (width < 768) {
          const box = await link.boundingBox();
          assert.ok(box.width >= 44 && box.height >= 44, 'Shortcut touch target too small');
        }
      }
      const shortcut = page.locator('.card-view-action').first();
      // Reach the link with Tab from the preceding Archive/Restore control.
      await page.locator('.archive-action').first().focus();
      await page.keyboard.press('Tab');
      assert.ok(await shortcut.evaluate(el => document.activeElement === el));
      assert.ok(await shortcut.evaluate(el => {
        const style = getComputedStyle(el);
        return style.outlineStyle !== 'none' && parseFloat(style.outlineWidth) > 0;
      }), 'Shortcut missing keyboard focus outline');
      if (process.env.JAWA_CARD_SCREENSHOT_DIR) {
        await page.screenshot({ path: `${process.env.JAWA_CARD_SCREENSHOT_DIR}/${view}-${width}.png`, fullPage: true });
      }
      const destination = new URL(await shortcut.getAttribute('href'), page.url()).href;
      for (const activation of ['Enter', 'click']) {
        const requests = [];
        const record = req => { if (req.isNavigationRequest() || req.method() === 'POST') requests.push([req.method(), req.url()]); };
        page.on('request', record);
        await Promise.all([
          page.waitForURL(destination),
          activation === 'Enter' ? shortcut.press('Enter') : shortcut.click(),
        ]);
        page.off('request', record);
        assert.deepEqual(requests, [['GET', destination]], 'Shortcut must only navigate, not archive or start work');
        await page.goBack();
        assert.equal(await page.locator('.drag-chosen, .drag-ghost').count(), 0);
      }
      console.log(`${view} ${width}: overflow, shortcut visibility, focus, target and navigation passed`);
    }
  }
  // A native link still works when JavaScript (including drag handlers) is disabled.
  const context = await browser.newContext({ javaScriptEnabled: false });
  const nativePage = await context.newPage();
  await nativePage.goto(`http://127.0.0.1:${server.address().port}/board`);
  const nativeLink = nativePage.locator('.card-view-action').first();
  const destination = new URL(await nativeLink.getAttribute('href'), nativePage.url()).href;
  await Promise.all([nativePage.waitForURL(destination), nativeLink.click()]);
  await context.close();
} finally {
  if (browser) await browser.close();
  await new Promise(resolve => server.close(resolve));
}
