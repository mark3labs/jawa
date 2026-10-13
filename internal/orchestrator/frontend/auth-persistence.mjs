import { launchPersistentContext } from './browser.mjs';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// Destructive account/NATS setup: explicitly supply a fresh disposable server.
const base = process.env.JAWA_SMOKE_URL;
assert.ok(base, 'Set JAWA_SMOKE_URL to a fresh disposable server');
const origin = new URL(base).origin;
const profile = await mkdtemp(join(tmpdir(), 'jawa-auth-profile-'));
let context;
const launch = async () => {
  context = await launchPersistentContext(profile, { timeout: 20000 });
  context.setDefaultTimeout(20000);
  context.setDefaultNavigationTimeout(20000);
  return context.pages()[0] || await context.newPage();
};
const controlled = async page => {
  await page.waitForFunction(() => navigator.serviceWorker.controller !== null);
};
const publicAssets = [
  '/assets/offline.html', '/assets/style.css', '/assets/app.js',
  '/assets/inter.woff2', '/assets/favicon.svg', '/assets/manifest.webmanifest',
  '/assets/icon-180.png', '/assets/icon-192.png', '/assets/icon-512.png',
  '/assets/icon-maskable-512.png',
];
const checkCache = async page => {
  const cached = await page.evaluate(async () => {
    const urls = [];
    for (const name of await caches.keys()) {
      for (const request of await (await caches.open(name)).keys()) urls.push(request.url);
    }
    return urls;
  });
  assert.ok(cached.length >= publicAssets.length, 'public assets were not precached');
  assert.ok(cached.every(value => {
    const url = new URL(value);
    return url.origin === origin && !url.search && publicAssets.includes(url.pathname);
  }), 'cache contains non-public content');
};
try {
  let page = await launch();
  await page.goto(`${origin}/setup`);
  assert.equal(new URL(page.url()).pathname, '/setup', 'server must have a fresh data directory');
  await page.getByLabel('Username', { exact: true }).fill('auth-persistence-admin');
  await page.getByLabel('Password', { exact: true }).fill('disposable-auth-password-123');
  await page.getByRole('button', { name: 'Create administrator' }).click();
  await page.locator('#settings-content').waitFor();
  await page.getByLabel('Username', { exact: true }).fill('auth-persistence-worker');
  await page.getByLabel('New password').fill('disposable-nats-password-123');
  await page.getByRole('button', { name: 'Save credentials and continue' }).click();
  await page.locator('#board-content').waitFor();
  await controlled(page);
  const issued = await context.cookies(origin);
  const expiries = {};
  for (const name of ['session', 'csrf']) {
    const c = issued.find(c => c.name === name);
    assert.ok(c, `${name} cookie missing`);
    assert.ok(Math.abs(c.expires - Date.now() / 1000 - 2592000) < 120, `${name} expiry is not 30 days`);
    assert.equal(c.httpOnly, name === 'session');
    assert.equal(c.sameSite, 'Strict');
    assert.equal(c.path, '/');
    expiries[name] = c.expires;
  }
  await page.reload();
  await page.locator('#board-content').waitFor();
  await checkCache(page);
  const manifest = await (await context.request.get(`${origin}/assets/manifest.webmanifest`)).json();
  const start = new URL(manifest.start_url, origin);
  assert.equal(start.origin, origin);
  await context.close();
  context = null;
  page = await launch();
  await page.goto(start.href);
  await page.locator('#board-content').waitFor();
  await controlled(page);
  const retained = await context.cookies(origin);
  for (const name of ['session', 'csrf']) {
    assert.equal(retained.find(c => c.name === name)?.expires, expiries[name], `${name} expiry changed on relaunch`);
  }
  const protectedRead = await page.evaluate(async () => {
    const response = await fetch('/activity');
    return { status: response.status, redirected: response.redirected, type: response.headers.get('content-type') };
  });
  assert.equal(protectedRead.status, 200);
  assert.equal(protectedRead.redirected, false);
  assert.match(protectedRead.type, /application\/json/);
  // Read the double-submit CSRF cookie only for this protected action; never log it.
  const logout = await page.evaluate(async () => {
    const csrf = document.cookie.split('; ').find(c => c.startsWith('csrf=')).slice(5);
    const response = await fetch('/logout', { method: 'POST', headers: { 'X-CSRF-Token': csrf } });
    return { status: response.status, path: new URL(response.url).pathname };
  });
  assert.equal(logout.status, 200);
  assert.equal(logout.path, '/login');
  await checkCache(page);
  await context.close();
  context = null;
  page = await launch();
  await page.goto(start.href);
  await page.getByRole('heading', { name: 'Welcome back' }).waitFor();
  assert.equal(new URL(page.url()).pathname, '/login');
  await controlled(page);
  await checkCache(page);
  console.log('PASS: 30-day cookies, reload, persistent Chromium profile/service-worker relaunch, protected read, logout persistence and public-only cache (not native installed-PWA coverage)');
} finally {
  try { await context?.close(); } finally { await rm(profile, { recursive: true, force: true }); }
}
