import { launchBrowser } from './browser.mjs';
import assert from 'node:assert/strict';

const base = process.env.JAWA_SMOKE_URL;
assert.ok(base, 'Set JAWA_SMOKE_URL to a disposable live server');
const browser = await launchBrowser();
try {
  const context = await browser.newContext();
  const page = await context.newPage();
  await page.goto(`${base}/login`);
  await page.evaluate(() => navigator.serviceWorker.ready);
  await page.waitForFunction(() => navigator.serviceWorker.controller);
  assert.equal(await page.locator('link[rel="manifest"]').getAttribute('href'), '/assets/manifest.webmanifest');
  const manifest = await (await context.request.get(`${base}/assets/manifest.webmanifest`)).json();
  assert.equal(manifest.display, 'standalone');
  for (const icon of manifest.icons) assert.equal((await context.request.get(base + icon.src)).status(), 200);
  await page.evaluate(async () => {
    await fetch('/snapshot');
    await fetch('/projects/issues');
    await fetch('/logout', {method: 'POST'});
  });
  await page.reload();
  const cached = await page.evaluate(async () => {
    const result = [];
    for (const name of await caches.keys()) {
      if (!name.startsWith('jawa-public-')) continue;
      for (const request of await (await caches.open(name)).keys()) result.push(new URL(request.url).pathname);
    }
    return result;
  });
  assert.ok(cached.includes('/assets/offline.html'));
  assert.ok(cached.length >= 10);
  assert.ok(cached.every(path => path.startsWith('/assets/')), `private data cached: ${cached}`);
  await context.setOffline(true);
  await page.goto(`${base}/board`);
  await page.getByRole('heading', {name: "You're offline"}).waitFor();
  assert.equal(await page.locator('#board-content').count(), 0);
  assert.equal(await page.locator('form').count(), 0);
  await context.setOffline(false);
  await page.getByRole('link', {name: 'Try again'}).click();
  await page.getByRole('heading', {name: 'Welcome back'}).waitFor();
  console.log('PASS: root-scoped PWA registration, manifest/icons, public-only cache, offline fallback and recovery');
} finally {
  await browser.close();
}
