import { launchBrowser } from './browser.mjs';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdtemp, rm } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

// Export actual Go-rendered pages; no hand-maintained dialog or composer replica.
const dir = await mkdtemp(join(tmpdir(), 'jawa-guidance-'));
const fixture = join(dir, 'guidance.html');
execFileSync('go', ['test', '../../../internal/orchestrator', '-run', '^TestGuidanceBrowserFixture$', '-count=1'], {
  env: { ...process.env, JAWA_GUIDANCE_HTML: fixture }, stdio: 'inherit',
});
const html = await readFile(fixture, 'utf8');
const patches = Object.fromEntries(await Promise.all(['pending', 'queued', 'next'].map(async state => [state, await readFile(`${fixture}.${state}`, 'utf8')])));
const elements = content => `event: datastar-patch-elements\ndata: elements ${content.replaceAll("\r", "").replaceAll("\n", "\ndata: elements ")}\n\n`;
const requests = [];
let reject = true;
let release;
const server = createServer(async (req, res) => {
  if (req.url.startsWith('/assets/')) {
    const name = new URL(req.url, 'http://localhost').pathname.split('/').pop();
    try {
      const asset = await readFile(new URL(`../assets/${name}`, import.meta.url));
      res.writeHead(200, { 'Content-Type': name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'application/octet-stream' }).end(asset);
    } catch { res.writeHead(404).end(); }
  } else if (req.url.startsWith('/events')) {
    // Keep the production stream idle; finite /patch requests exercise live morphs.
    res.writeHead(200, { 'Content-Type': 'text/event-stream' }); res.write(': connected\n\n');
  } else if (req.url.startsWith('/patch')) {
    const state = new URL(req.url, 'http://localhost').searchParams.get('state');
    res.writeHead(200, { 'Content-Type': 'text/event-stream' }).end(elements(patches[state]));
  } else if (req.method === 'POST') {
    let body = '';
    for await (const part of req) body += part;
    requests.push({ url: req.url, form: Object.fromEntries(new URLSearchParams(body)) });
    // Hold acceptance so closure cannot be caused by submit/start.
    await new Promise(resolve => { release = resolve; });
    res.writeHead(200, { 'Content-Type': 'text/event-stream' }).end(reject
      ? elements('<div id="notice">Guidance rejected</div>')
      : elements(patches.queued) + 'event: datastar-patch-signals\ndata: signals {"guideOpen":false,"guideCard":"","guideToken":"","guideMessage":""}\n\n');
  } else res.writeHead(200, { 'Content-Type': 'text/html' }).end(html);
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser;
try {
  browser = await launchBrowser();
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.goto(`http://127.0.0.1:${server.address().port}/planning?card=card`);
  // Add only test drivers; all exercised markup is production templ output.
  await page.evaluate(() => {
    const button = document.createElement('button');
    button.id = 'open-guide'; button.textContent = 'Open guidance';
    button.setAttribute('data-on:click', "$guideCard = 'card'; $guideToken = 'report-token'; $guideMessage = ''; $guideOpen = true");
    document.body.append(button);
    for (const state of ['pending', 'next']) {
      const patch = document.createElement('button'); patch.id = `patch-${state}`; patch.textContent = state;
      patch.setAttribute('data-on:click', `@get('/patch?state=${state}')`); document.body.append(patch);
    }
  });
  const waitSubmission = async count => {
    const deadline = Date.now() + 5000;
    while (requests.length < count && Date.now() < deadline) await delay(10);
    assert.equal(requests.length, count, 'bounded wait for submission');
  };
  const dialog = page.locator('#guide-dialog');
  const modalMessage = page.locator('#guide-message');
  await page.click('#open-guide');
  await dialog.waitFor({ state: 'visible' });
  await modalMessage.fill('Saved guidance');
  const sendModal = dialog.getByRole('button', { name: 'Send guidance', exact: true });
  await sendModal.click();
  await waitSubmission(1);
  assert.equal(await dialog.isVisible(), true, 'not closed before durable acceptance');
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, '/cards/guide');
  assert.equal(requests[0].form.expected_interaction, 'report-token');
  assert.equal(requests[0].form.message, 'Saved guidance');
  assert.equal(requests[0].form.card_id, 'card');
  assert.equal(requests[0].form._csrf, 'test-csrf');
  assert.equal(requests[0].form.view, 'planning');
  assert.equal(requests[0].form.card, 'card');
  release();
  await page.locator('#notice').filter({ hasText: 'Guidance rejected' }).waitFor();
  assert.equal(await dialog.isVisible(), true);
  assert.equal(await modalMessage.inputValue(), 'Saved guidance', 'rejection retains draft');
  reject = false;
  await sendModal.click();
  await waitSubmission(2);
  assert.equal(await dialog.isVisible(), true);
  release();
  await page.waitForFunction(() => !document.querySelector('#guide-dialog').open);
  assert.equal(await modalMessage.inputValue(), '');
  await page.locator('#workflow-message-report-token-user').filter({ hasText: 'Saved guidance' }).waitFor();
  assert.equal(await page.locator('[id^=guidance-reply-form-] textarea').count(), 0, 'queued composer removed');

  // A fresh page retains inline drafts through repeated unsolicited patches.
  await page.reload();
  await page.evaluate(() => {
    for (const state of ['pending', 'next']) {
      const patch = document.createElement('button'); patch.id = `patch-${state}`; patch.textContent = state;
      patch.setAttribute('data-on:click', `@get('/patch?state=${state}')`); document.body.append(patch);
    }
  });
  if (process.env.JAWA_GUIDANCE_SCREENSHOT_DIR) {
    for (const width of [320, 390, 1440]) {
      await page.setViewportSize({ width, height: 900 });
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, `pending overflow at ${width}`);
      await page.screenshot({ path: join(process.env.JAWA_GUIDANCE_SCREENSHOT_DIR, `guidance-pending-${width}.png`), fullPage: true });
    }
  }
  await page.locator('[id^=guidance-reply-form-] textarea').fill('Inline draft');
  for (let i = 0; i < 3; i++) {
    await page.click('#patch-pending');
    await page.waitForTimeout(100);
    assert.equal(await page.locator('[id^=guidance-reply-form-] textarea').inputValue(), 'Inline draft');
  }
  await page.click('#patch-next');
  await page.locator('#guidance-reply-form-next-token').waitFor();
  assert.equal(await page.locator('#guidance-reply-form-report-token').count(), 0);
  assert.equal(await page.locator('[id^=guidance-reply-form-] textarea').inputValue(), '', 'old draft cannot attach to new token');
  await page.locator('[id^=guidance-reply-form-] textarea').fill('New blocker guidance');
  await page.locator('[id^=guidance-reply-form-] textarea').press('Tab');
  await page.keyboard.press('Enter');
  await waitSubmission(3);
  assert.equal(requests.at(-1).form.expected_interaction, 'next-token');
  assert.equal(requests.at(-1).form.message, 'New blocker guidance');
  release();
  await page.locator('[id^=guidance-reply-form-] textarea').waitFor({ state: 'detached' });
  for (const width of [320, 390, 1440]) {
    await page.setViewportSize({ width, height: 900 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, `overflow at ${width}`);
    if (process.env.JAWA_GUIDANCE_SCREENSHOT_DIR) {
      await page.screenshot({ path: join(process.env.JAWA_GUIDANCE_SCREENSHOT_DIR, `guidance-${width}.png`), fullPage: true });
    }
  }
  assert.deepEqual(errors, []);
  console.log('Production guidance modal acceptance/rejection, inline tokens, draft morphing, keyboard and responsive checks passed');
} finally {
  release?.();
  await browser?.close();
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
  await rm(dir, { recursive: true, force: true });
}
