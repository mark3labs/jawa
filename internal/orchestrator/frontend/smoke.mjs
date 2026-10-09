import { chromium } from 'playwright';
import assert from 'node:assert/strict';
// playwright (without @playwright/test) supplies locators, not expect.
const expect = locator => {
 const check = async predicate => {
  const deadline = Date.now() + 20000;
  while (Date.now() < deadline) {
   if (await predicate()) return;
   await new Promise(resolve => setTimeout(resolve, 100));
  }
  assert.fail(`Locator assertion timed out: ${locator}`);
 };
 return {
  toBeDisabled: () => check(() => locator.isDisabled()),
  toBeVisible: () => locator.waitFor({state: 'visible'}),
  toHaveValue: value => check(async () => await locator.inputValue() === value),
  toHaveCount: count => check(async () => await locator.count() === count),
 };
};
import { mkdir } from 'node:fs/promises';
import { dirname } from 'node:path';

// Run via go run ./cmd/factory-smoke: its private loopback bridge owns a real
// BONNIE/Kit worker. No protocol outcomes or presence records are faked here.
const base = process.env.JAWA_SMOKE_URL || 'http://127.0.0.1:18080';
const control = process.env.JAWA_SMOKE_CONTROL;
assert.ok(control, 'Run go run ./cmd/factory-smoke to supply the offline BONNIE worker');
const worker = 'factory-smoke-worker';
const pr = 'https://github.com/example/smoke/pull/1';
const browser = await chromium.launch({
 executablePath: process.env.CHROMIUM_PATH || '/etc/profiles/per-user/space_cowboy/bin/chromium',
 headless: true, args: ['--no-sandbox'],
});
const page = await browser.newPage({viewport: {width: 1440, height: 1000}});
page.setDefaultTimeout(20000);
// Observe the real browser stream without replacing fetch or manufacturing frames.
await page.addInitScript(() => {
 window.testToken = crypto.randomUUID();
 window.documentInitCount = Number(sessionStorage.smokeInitCount || 0) + 1;
 sessionStorage.smokeInitCount = window.documentInitCount;
 window.smokeSSE = {frames: '', contentType: '', ended: false};
 const fetch = window.fetch.bind(window);
 window.fetch = async (...args) => {
  const response = await fetch(...args);
  if (new URL(response.url).pathname === '/events') {
   window.smokeSSE.contentType = response.headers.get('content-type');
   const reader = response.clone().body.getReader();
   const decoder = new TextDecoder();
   (async () => {
    try {
     while (true) {
      const {value, done} = await reader.read();
      if (done) { window.smokeSSE.ended = true; break; }
      window.smokeSSE.frames += decoder.decode(value, {stream: true});
     }
    } catch { window.smokeSSE.ended = true; }
   })();
  }
  return response;
 };
});
const browserActivityRequests = [];
page.on('request', r => {
 if (new URL(r.url()).pathname === '/activity') browserActivityRequests.push(r.url());
});
const errors = [];
const uiFailures = [];
page.on('pageerror', e => errors.push(e.message));
const card = title => page.locator('.task-card').filter({hasText: title});
const laneCard = (lane, title) => page.locator(`.task-list[data-status="${lane}"] .task-card`).filter({hasText: title});
const bridge = async path => {
 const response = await page.request.post(control + path, {headers: {Authorization: `Bearer ${process.env.JAWA_SMOKE_CONTROL_TOKEN}`}});
 assert.equal(response.status(), 204, `worker control ${path} failed`);
};
const activity = async () => {
 const response = await page.request.get(base + '/activity');
 assert.equal(response.status(), 200);
 return response.json();
};
async function waitActivity(predicate, description) {
 const deadline = Date.now() + 25000;
 while (Date.now() < deadline) {
  const data = await activity();
  if (predicate(data)) return data;
  await page.waitForTimeout(150);
 }
 assert.fail(`Timed out waiting for ${description}`);
}
async function drag(title, lane) {
 const handle = await card(title).locator('.drag-handle').boundingBox();
 const target = await page.locator(`.task-list[data-status="${lane}"]`).boundingBox();
 assert.ok(handle && target);
 await page.mouse.move(handle.x + handle.width / 2, handle.y + handle.height / 2);
 await page.mouse.down();
 await page.mouse.move(handle.x + 20, handle.y + 20, {steps: 8});
 await page.waitForTimeout(200);
 await page.mouse.move(target.x + target.width / 2, target.y + target.height - 25, {steps: 25});
 await page.waitForTimeout(300);
 await page.mouse.up();
 await laneCard(lane, title).waitFor();
}
let started = false;
try {
 await page.goto(base + '/setup');
 await page.getByLabel('Username', {exact: true}).fill('smoke-admin');
 await page.getByLabel('Password', {exact: true}).fill('smoke-test-password-123');
 await page.getByRole('button', {name: 'Create administrator'}).click();
 await page.getByRole('heading', {name: 'Project board'}).waitFor();
 // Rotate before connecting the worker; internal workflow credentials survive.
 await page.getByText('Worker connection settings').click();
 await page.getByLabel('NATS username').fill(process.env.JAWA_SMOKE_WORKER_USER);
 await page.getByLabel('New NATS password').fill(process.env.JAWA_SMOKE_WORKER_PASSWORD);
 await page.getByRole('button', {name: 'Rotate credentials'}).click();
 await page.getByRole('heading', {name: 'Project board'}).waitFor();


 await page.getByRole('button', {name: 'New project'}).click();
 await page.getByLabel('Name', {exact: true}).fill('Smoke project');
 await page.getByLabel('Repository URL').fill('https://github.com/example/smoke.git');
 await page.getByRole('button', {name: 'Create project', exact: true}).click();
 await page.getByRole('heading', {name: 'Smoke project'}).waitFor();
 const documentState = await page.evaluate(() => ({token: window.testToken, count: window.documentInitCount}));
 const sameDocument = async () => assert.deepEqual(await page.evaluate(() => ({token: window.testToken, count: window.documentInitCount})), documentState);
 await bridge('/seed-legacy');
 await laneCard('Building', 'Legacy Building card').waitFor();
 assert.equal((await activity()).attempts.length, 0);
 await bridge('/start'); started = true;
 await waitActivity(d => d.workers.some(w => w.identity.worker === worker && w.state === 'ready' && w.endpoints.some(e => e.input && e.ready)), 'real BONNIE presence');
 await page.waitForFunction(() => window.smokeSSE.contentType?.startsWith('text/event-stream') && window.smokeSSE.frames.includes('event: datastar-patch-elements'));

 for (const title of ['First smoke card', 'Second smoke card']) {
  await page.getByRole('button', {name: 'Add card'}).click();
  await page.getByLabel('Title', {exact: true}).fill(title);
  await page.getByLabel('Description').fill('Offline browser orchestration test');
  await page.getByRole('button', {name: 'Create card', exact: true}).click();
  await page.getByRole('heading', {name: title}).waitFor();
 }
 const firstID = await card('First smoke card').getAttribute('data-card-id');
 await card('First smoke card').getByRole('combobox').selectOption('Building');
 await laneCard('Building', 'First smoke card').waitFor();
 await waitActivity(d => d.attempts.some(a => a.CardID === firstID && a.State === 'running' && a.WorkerID === worker && a.RunID), 'worker pickup/running');
 // Do not let missing UI mount points mask the transport/result assertions.
 // Live activity is mandatory, not an optional transport-only pass.
 const activityMounted = await page.locator('[data-activity]').count();
 assert.equal(activityMounted, 1);
 await card('First smoke card').locator('[data-attempt-badge]').filter({hasText: 'running'}).waitFor();
 await expect(card('First smoke card').getByRole('button', {name: 'Delete', exact: true})).toBeDisabled();
 await expect(card('First smoke card').getByRole('button', {name: 'Reset to Todo', exact: true})).toBeDisabled();
 // A genuine Sortable pointer drag starts a second execution, not manual Done.
 await drag('Second smoke card', 'Building');
 await card('Legacy Building card').getByRole('button', {name: 'Start work', exact: true}).click();
 await card('Legacy Building card').locator('[data-attempt-badge]').filter({hasText: 'running'}).waitFor();
 await expect(card('Legacy Building card').getByRole('button', {name: 'Delete', exact: true})).toBeDisabled();
 await expect(card('Legacy Building card').getByRole('button', {name: 'Reset to Todo', exact: true})).toBeDisabled();
 // Live result patches must not close or replace a user's card draft.
 await page.getByRole('button', {name: 'Add card'}).click();
 await page.getByLabel('Title', {exact: true}).fill('Preserved draft');
 await page.getByLabel('Description').fill('Keep this while results arrive');
 const data = await waitActivity(d => d.attempts.length === 3 && d.attempts.every(a => a.Result && a.Error === 'provider: credentials missing'), 'real BONNIE outcomes and fail-closed provider verification');
 await card('Legacy Building card').locator('[data-card-attempts] > .notice').filter({hasText: 'provider: credentials missing'}).waitFor();
 await expect(page.locator('#card-dialog')).toBeVisible();
 await expect(page.getByLabel('Title', {exact: true})).toHaveValue('Preserved draft');
 await expect(page.getByLabel('Description')).toHaveValue('Keep this while results arrive');
 await page.locator('#card-dialog').getByRole('button', {name: 'Cancel', exact: true}).click();
 await sameDocument();
 for (const attempt of data.attempts) {
  assert.equal(attempt.Published, true);
  assert.equal(attempt.WorkerID, worker);
  assert.ok(attempt.RunID && attempt.RemoteAttemptID && attempt.OutcomeJSON);
  assert.equal(attempt.RunState, 'completed');
  assert.equal(attempt.PRURL, pr);
  assert.equal(JSON.parse(attempt.Result).pr_url, pr);
  assert.equal(attempt.Ready, false);
  assert.equal(attempt.Number, 1);
  assert.ok(attempt.TaskJSON.includes(`Branch: jawa/card/${attempt.CardID}/attempt/1`));
 }
 assert.ok(data.cards.every(c => c.Status === 'Building'));
 console.log('PASS: real BONNIE presence, bonnie.tasks publishes, running pickups, three Kit final JSON outcomes, production provider blocker; all three cards remain Building.');
 await page.locator('#activity-workers').getByText(`${worker} · Available (advertised)`, {exact: true}).waitFor();

 for (const title of ['First smoke card', 'Second smoke card']) {
  await card(title).locator('[data-card-attempts] > .notice').filter({hasText: 'provider: credentials missing'}).waitFor();
  assert.equal(await card(title).locator('[data-card-attempts] > a').getAttribute('href'), pr);
  await card(title).locator('[data-attempt-history] > summary').click();
  await card(title).locator('[data-attempt-id] > summary').click();
  await card(title).locator('[data-attempt-id] .notice').waitFor({state: 'visible'});
 }
 // Manual Done remains blocked; errors patch the banner, not navigation.
 const rejected = page.waitForResponse(r => r.url().endsWith('/cards/move') && r.request().method() === 'POST');
 await card('Second smoke card').getByRole('combobox').selectOption('Done');
 assert.equal((await rejected).status(), 200, 'Datastar errors are finite SSE banner responses');
 assert.match(await (await rejected).text(), /event: datastar-patch-elements/);
 await page.locator('#notice').filter({hasText: 'done requires verified PR readiness'}).waitFor({state: 'visible'});
 assert.equal(await laneCard('Building', 'Second smoke card').count(), 1);
 assert.equal(await page.locator('.task-list[data-status="Done"] .task-card').count(), 0);
 await page.getByLabel('Filter cards').fill('First');
 await page.waitForTimeout(200);
 assert.equal(await page.locator('.task-card:visible').count(), 1);
 await page.getByLabel('Filter cards').fill('');
 // Expanded keyed nodes and filter survive a real unsolicited worker patch.
 const historyID = await card('First smoke card').locator('[data-attempt-history]').getAttribute('id');
 const attemptID = await card('First smoke card').locator('[data-attempt-id]').getAttribute('id');
 // Require both disclosure levels open before the unsolicited SSE update.
 assert.equal(await page.locator('#' + historyID).evaluate(el => el.open), true);
 assert.equal(await page.locator('#' + attemptID).evaluate(el => el.open), true);
 await page.locator('#' + historyID).evaluate(el => el.smokeIdentity = 'stable-history');
 await page.getByLabel('Filter cards').fill('First');
 await bridge('/stop'); started = false;
 await waitActivity(d => d.workers.length === 0, 'presence removal after shutdown');
 await page.locator('#activity-workers').getByText('No live agents reported.').waitFor();
 await expect(page.getByLabel('Filter cards')).toHaveValue('First');
 assert.equal(await page.locator('.task-card:visible').count(), 1);
 const historyState = await page.locator('#' + historyID).evaluate(el => ({open: el.open, stable: el.smokeIdentity === 'stable-history'}));
 const attemptOpen = await page.locator('#' + attemptID).evaluate(el => el.open);
 if (!attemptOpen) uiFailures.push('SSE patch must preserve expanded attempt detail');
 if (!historyState.open || !historyState.stable) {
  uiFailures.push(`SSE patch must preserve expanded keyed history: ${JSON.stringify(historyState)}`);
  console.error('UI FAILURE:', uiFailures.at(-1));
 }
 // The project dialog is also outside patch roots and must retain its draft.
 await page.getByRole('button', {name: 'New project'}).click();
 await page.getByLabel('Name', {exact: true}).fill('Project draft survives SSE');
 await page.getByLabel('Repository URL').fill('https://github.com/example/draft.git');
 await bridge('/start'); started = true;
 await waitActivity(d => d.workers.some(w => w.identity.worker === worker && w.state === 'ready'), 'restarted worker');
 await page.locator('#activity-workers').getByText(`${worker} · Available (advertised)`, {exact: true}).waitFor();
 await expect(page.locator('#project-dialog')).toBeVisible();
 await expect(page.getByLabel('Name', {exact: true})).toHaveValue('Project draft survives SSE');
 await expect(page.getByLabel('Repository URL')).toHaveValue('https://github.com/example/draft.git');
 await page.locator('#project-dialog').getByRole('button', {name: 'Cancel', exact: true}).click();
 await page.getByLabel('Filter cards').fill('');
 await sameDocument();
 // Reset retains immutable history; Delete requires an explicit confirmation.
 const legacyHistory = (await activity()).attempts.filter(a => a.CardID === 'smoke-legacy');
 await card('Legacy Building card').getByRole('button', {name: 'Reset to Todo', exact: true}).click();
 await laneCard('Todo', 'Legacy Building card').waitFor();
 assert.deepEqual((await activity()).attempts.filter(a => a.CardID === 'smoke-legacy'), legacyHistory);
 await card('Legacy Building card').getByRole('button', {name: 'Delete', exact: true}).click();
 await expect(page.locator('#delete-dialog')).toBeVisible();
 assert.equal(await card('Legacy Building card').count(), 1);
 await page.getByRole('button', {name: 'Keep card', exact: true}).click();
 assert.equal(await card('Legacy Building card').count(), 1);
 await card('Legacy Building card').getByRole('button', {name: 'Delete', exact: true}).click();
 await page.getByRole('button', {name: 'Delete permanently', exact: true}).click();
 await expect(card('Legacy Building card')).toHaveCount(0);
 assert.equal((await activity()).attempts.length, 2);
 await sameDocument();
 assert.deepEqual(browserActivityRequests, [], 'browser must not poll /activity');
 assert.equal(await page.evaluate(() => window.smokeSSE.ended), false, '/events stays open through mutations and worker shutdown');
 assert.ok(await page.evaluate(() => window.smokeSSE.frames.split('event: datastar-patch-elements').length > 2));
 const screenshot = process.env.JAWA_SMOKE_SCREENSHOT || '/tmp/jawa-factory-smoke.png';
 await mkdir(dirname(screenshot), {recursive: true});
 await page.screenshot({path: screenshot, fullPage: true});
 await page.setViewportSize({width: 390, height: 844});
 await page.screenshot({path: screenshot.replace(/\.png$/, '-mobile.png'), fullPage: true});
 await page.setViewportSize({width: 1440, height: 1000});
 console.log(`Screenshots: ${screenshot}, ${screenshot.replace(/\.png$/, '-mobile.png')}`);

 // Graceful outage exercises BONNIE draining/unregister, not a fixture KV delete.
 await bridge('/stop'); started = false;
 await waitActivity(d => d.workers.length === 0, 'presence removal after shutdown');
 await page.locator('#activity-workers').getByText('No live agents reported.').waitFor();
 assert.equal((await activity()).attempts.length, 2, 'history survives worker outage');
 await page.getByRole('button', {name: 'Sign out'}).click();
 assert.equal((await page.request.get(base + '/activity', {maxRedirects: 0})).status(), 303);
 await page.getByLabel('Username', {exact: true}).fill('smoke-admin');
 await page.getByLabel('Password', {exact: true}).fill('smoke-test-password-123');
 await page.getByRole('button', {name: 'Sign in', exact: true}).click();
 await page.getByRole('heading', {name: 'Project board'}).waitFor();
 assert.equal(await page.locator('.task-list[data-status="Building"] .task-card').count(), 2);
 assert.deepEqual(browserActivityRequests, [], 'no browser /activity polling including re-login');
 assert.deepEqual(errors, []);
 console.log('Completed: SSE stream/no polling/no reload, draft preservation, guards, Reset/history, Delete confirmation, screenshots, outage and re-login.');
 assert.deepEqual(uiFailures, [], 'Parent UI fixes required (assertions retained; remaining harness completed)');
 console.log('PASS: rejected manual Done/DOM rollback, Datastar filtering, persistence, BONNIE shutdown/discovery removal, logout/login; no browser errors.');
 console.log('PASS: setup, credential rotation, real BONNIE presence, project/cards, keyboard + pointer Building, published tasks, running pickups, final PR results/history, provider credential blocker, rejected manual Done/DOM rollback, filtering, persistence, worker shutdown/discovery removal, logout/login; no browser errors or external API calls.');
} finally {
 if (started) await bridge('/stop').catch(() => {});
 await browser.close();
}
