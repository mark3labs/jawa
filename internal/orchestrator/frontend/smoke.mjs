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
// BONNIE/Kit agent. No protocol outcomes or presence records are faked here.
const base = process.env.JAWA_SMOKE_URL || 'http://127.0.0.1:18080';
const control = process.env.JAWA_SMOKE_CONTROL;
assert.ok(control, 'Run go run ./cmd/factory-smoke to supply the offline BONNIE agent');
const agent = 'factory-smoke-agent';
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
   window.smokeSSE.url = response.url;
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

page.on('pageerror', e => errors.push(e.message));
const card = title => page.locator('.task-card').filter({hasText: title});
const laneCard = (lane, title) => page.locator(`.task-list[data-status="${lane}"] .task-card`).filter({hasText: title});
const bridge = async path => {
 const response = await page.request.post(control + path, {headers: {Authorization: `Bearer ${process.env.JAWA_SMOKE_CONTROL_TOKEN}`}});
 assert.equal(response.status(), 204, `agent control ${path} failed`);
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
async function drag(title, lane, accepted = true) {
 const handle = await card(title).locator('.card-title').boundingBox();
 const target = await page.locator(`.task-list[data-status="${lane}"]`).boundingBox();
 assert.ok(handle && target);
 await page.mouse.move(handle.x + handle.width / 2, handle.y + handle.height / 2);
 await page.mouse.down();
 await page.mouse.move(handle.x + 20, handle.y + 20, {steps: 8});
 await page.waitForTimeout(200);
 await page.mouse.move(target.x + target.width / 2, target.y + target.height - 25, {steps: 25});
 await page.waitForTimeout(300);
 await page.mouse.up();
 if (accepted) await laneCard(lane, title).waitFor();
}
// Actions are native popover menuitems, not always-visible buttons.
async function menu(title) {
 await card(title).getByRole('button', {name: `Actions for ${title}`, exact: true}).click();
 await card(title).locator('[popover]').waitFor({state: 'visible'});
 return card(title).locator('[popover]');
}
async function action(title, name) {
 await (await menu(title)).getByRole('menuitem', {name, exact: true}).click();
}
async function guards(title) {
 const m = await menu(title);
 for (const name of ['Delete', 'Reset to Todo']) {
  assert.equal(await m.getByRole('menuitem', {name: new RegExp(`^${name}`)}).getAttribute('aria-disabled'), 'true');
 }
 await page.keyboard.press('Escape');
}
async function screen(path, view) {
 await page.goto(base + path);
 await page.locator(`#${view}-content`).waitFor();
 await page.waitForFunction(() => window.smokeSSE.frames.includes('event: datastar-patch-elements'));
 for (const other of ['board', 'runs', 'agents', 'settings'].filter(v => v !== view)) {
  assert.equal(await page.locator(`#${other}-content`).count(), 0, `${other} must not appear on ${view}`);
 }
 await page.waitForFunction(() => customElements.get('jawa-shortcuts') && customElements.get('jawa-copy') && customElements.get('jawa-time') && customElements.get('jawa-menu') && customElements.get('jawa-board'));
}
async function stableDocument() {
 const state = await page.evaluate(() => {
  document.querySelector('.shell').smokeIdentity = 'shell';
  document.querySelector('#content').smokeIdentity = 'main';
  return {token: window.testToken, count: window.documentInitCount};
 });
 return async () => {
  assert.deepEqual(await page.evaluate(() => ({token: window.testToken, count: window.documentInitCount})), state);
  assert.equal(await page.locator('.shell').evaluate(el => el.smokeIdentity), 'shell');
  assert.equal(await page.locator('#content').evaluate(el => el.smokeIdentity), 'main');
  assert.equal(await page.evaluate(() => window.smokeSSE.ended), false);
  const frames = await page.evaluate(() => window.smokeSSE.frames);
  assert.doesNotMatch(frames, /<main|class="shell"|id="content"/);
 };
}
const newCard = () => page.locator('.topbar').getByRole('button', {name: 'New card', exact: true});
async function createCard(title) {
 await newCard().click();
 await page.getByLabel('Title', {exact: true}).fill(title);
 await page.getByLabel('Description').fill('Offline browser orchestration test');
 await page.getByRole('button', {name: 'Create card', exact: true}).click();
 await card(title).waitFor();
}
async function createProject(name) {
 await page.locator('#sidebar-nav').getByRole('button', {name: 'New project'}).click();
 await page.getByLabel('Name', {exact: true}).fill(name);
 await page.getByLabel('Repository URL').fill('https://github.com/example/smoke.git');
 await page.getByRole('button', {name: 'Create project', exact: true}).click();
 await page.getByRole('heading', {name, exact: true}).waitFor();
 return new URL(page.url()).searchParams.get('project');
}
// Provider re-verification refreshes UpdatedAt; task/result identity stays immutable.
const immutableHistory = attempts => attempts.map(({UpdatedAt, ...fields}) => fields);
let started = false;
try {
 await page.goto(base + '/setup');
 await page.getByLabel('Username', {exact: true}).fill('smoke-admin');
 await page.getByLabel('Password', {exact: true}).fill('smoke-test-password-123');
 await page.getByRole('button', {name: 'Create administrator'}).click();
 await page.locator('#board-content').waitFor();
 await screen('/settings', 'settings');
 const settingsStable = await stableDocument();
 await page.getByLabel('Username', {exact: true}).fill(process.env.JAWA_SMOKE_AGENT_USER);
 await page.getByLabel('New password').fill(process.env.JAWA_SMOKE_AGENT_PASSWORD);
 await page.getByRole('button', {name: 'Rotate credentials'}).click();
 await page.locator('#notice').filter({hasText: 'rotat'}).waitFor({state: 'visible'});
 await settingsStable();
 assert.equal(await page.locator('#providers').getByText('Not configured', {exact: true}).count(), 2);
 await screen('/board', 'board');
 const projectID = await createProject('Smoke project');
 assert.ok(projectID);
 const boardPath = `/board?project=${projectID}`;
 await bridge('/seed-legacy');
 await laneCard('Building', 'Legacy Building card').waitFor();
 assert.equal((await activity()).attempts.length, 0);
 await screen('/agents', 'agents');
 const agentsStable = await stableDocument();
 await bridge('/start'); started = true;
 await waitActivity(d => d.agents.some(w => w.identity.agent === agent && w.state === 'ready' && w.endpoints.some(e => e.input && e.ready)), 'real BONNIE presence');
 const agent = page.locator('#agent-' + agent);
 await agent.getByText('Available', {exact: true}).waitFor();
 await agent.getByText('Inspect record', {exact: true}).click();
 const record = JSON.parse(await agent.locator('details pre').textContent());
 assert.equal(record.identity.agent, agent);
 assert.ok(record.endpoints.some(e => e.input && e.ready));
 await agentsStable();
 await screen(boardPath, 'board');
 const sameDocument = await stableDocument();
 // Keyboard shortcuts work on the board and are inert while typing/in dialogs.
 await page.keyboard.press('c');
 await expect(page.locator('#card-dialog')).toBeVisible();
 await page.getByLabel('Title', {exact: true}).fill('');
 await page.getByLabel('Title', {exact: true}).pressSequentially('c/g r');
 await expect(page.getByLabel('Title', {exact: true})).toHaveValue('c/g r');
 assert.equal(new URL(page.url()).pathname, '/board');
 await page.locator('#card-dialog').getByRole('button', {name: 'Cancel', exact: true}).click();
 await page.locator('h1').click();
 await page.keyboard.press('/');
 assert.equal(await page.getByLabel('Filter cards').evaluate(el => el === document.activeElement), true);
 await page.getByLabel('Filter cards').pressSequentially('c/g r');
 await expect(page.getByLabel('Filter cards')).toHaveValue('c/g r');
 assert.equal(new URL(page.url()).pathname, '/board');
 await page.getByLabel('Filter cards').fill('');
 for (const title of ['First smoke card', 'Second smoke card']) await createCard(title);
 const firstID = await card('First smoke card').getAttribute('data-card-id');
 const secondID = await card('Second smoke card').getAttribute('data-card-id');
 await action('First smoke card', 'Start work');
 await laneCard('Building', 'First smoke card').waitFor();
 await waitActivity(d => d.attempts.some(a => a.CardID === firstID && a.State === 'running' && a.AgentID === agent && a.RunID), 'agent pickup/running');
 await card('First smoke card').getByRole('link', {name: 'Running', exact: true}).waitFor();
 await guards('First smoke card');
 await drag('Second smoke card', 'Building');
 await action('Legacy Building card', 'Start work');
 await card('Legacy Building card').getByRole('link', {name: 'Running', exact: true}).waitFor();
 await guards('Legacy Building card');
 // Unsolicited results may patch cards, but never the shell or open draft.
 await newCard().click();
 await page.getByLabel('Title', {exact: true}).fill('Preserved draft');
 await page.getByLabel('Description').fill('Keep this while results arrive');
 const data = await waitActivity(d => d.attempts.length === 3 && d.attempts.every(a => a.Result && a.Error === 'provider: credentials missing'), 'real BONNIE outcomes and fail-closed provider verification');
 await card('Legacy Building card').locator('.card-note').filter({hasText: 'provider: credentials missing'}).waitFor();
 await expect(page.locator('#card-dialog')).toBeVisible();
 await expect(page.getByLabel('Title', {exact: true})).toHaveValue('Preserved draft');
 await expect(page.getByLabel('Description')).toHaveValue('Keep this while results arrive');
 await page.locator('#card-dialog').getByRole('button', {name: 'Cancel', exact: true}).click();
 await sameDocument();
 for (const attempt of data.attempts) {
  assert.equal(attempt.Published, true);
  assert.equal(attempt.AgentID, agent);
  assert.ok(attempt.RunID && attempt.RemoteAttemptID && attempt.OutcomeJSON);
  assert.equal(attempt.RunState, 'completed');
  assert.equal(attempt.PRURL, pr);
  assert.equal(JSON.parse(attempt.Result).pr_number, 1);
  assert.equal(attempt.Ready, false);
  assert.equal(attempt.Number, 1);
  assert.ok(JSON.parse(attempt.TaskJSON).text.split('\nBranch: ')[1].endsWith(`-${attempt.CardID.slice(0,6)}-a1`));
 }
 assert.ok(data.cards.every(c => c.Status === 'Building'));
 for (const title of ['First smoke card', 'Second smoke card']) {
  await card(title).locator('.card-note').filter({hasText: 'provider: credentials missing'}).waitFor();
  assert.equal(await card(title).getByTitle('Open pull request').getAttribute('href'), pr);
 }
 // Done must be exercised with an actual pointer drag (there is no move select).
 const rejected = page.waitForResponse(r => new URL(r.url()).pathname === '/cards/move' && r.request().method() === 'POST').then(async response => ({status: response.status(), text: await response.text()}));
 await drag('Second smoke card', 'Done', false);
 const rejection = await rejected;
 assert.equal(rejection.status, 200, 'Datastar errors are finite SSE banner responses');
 assert.match(rejection.text, /event: datastar-patch-elements/);
 await page.locator('#notice').filter({hasText: 'done requires verified PR readiness'}).waitFor({state: 'visible'});
 assert.equal(await laneCard('Building', 'Second smoke card').count(), 1);
 assert.equal(await page.locator('.task-list[data-status="Done"] .task-card').count(), 0);
 await page.getByLabel('Filter cards').fill('First');
 await expect(page.locator('.task-card:visible')).toHaveCount(1);
 await bridge('/stop'); started = false;
 await waitActivity(d => d.agents.length === 0, 'presence removal after shutdown');
 await page.locator('#agent-status').getByText('No agents online').waitFor();
 await expect(page.getByLabel('Filter cards')).toHaveValue('First');
 await expect(page.locator('.task-card:visible')).toHaveCount(1);
 await sameDocument();
 // Project draft is outside patch roots as well.
 await page.locator('#sidebar-nav').getByRole('button', {name: 'New project'}).click();
 await page.getByLabel('Name', {exact: true}).fill('Project draft survives SSE');
 await page.getByLabel('Repository URL').fill('https://github.com/example/draft.git');
 await bridge('/start'); started = true;
 await waitActivity(d => d.agents.some(w => w.identity.agent === agent && w.state === 'ready'), 'restarted agent');
 await page.locator('#agent-status').getByText('1 agent online').waitFor();
 await expect(page.locator('#project-dialog')).toBeVisible();
 await expect(page.getByLabel('Name', {exact: true})).toHaveValue('Project draft survives SSE');
 await expect(page.getByLabel('Repository URL')).toHaveValue('https://github.com/example/draft.git');
 await page.locator('#project-dialog').getByRole('button', {name: 'Cancel', exact: true}).click();
 await page.getByLabel('Filter cards').fill('');
 await sameDocument();
 // G R navigation, card-specific history, state tabs and immutable disclosures.
 await page.locator('h1').click();
 await page.keyboard.press('g');
 await page.keyboard.press('r');
 await page.locator('#runs-content').waitFor();
 assert.equal(new URL(page.url()).pathname, '/runs');
 await expect(page.locator('details.run')).toHaveCount(3);
 await screen(`/runs?card=${firstID}`, 'runs');
 await expect(page.locator('details.run')).toHaveCount(1);
 const run = page.locator('details.run');
 await run.locator('summary').click();
 await run.locator('.card-note').filter({hasText: 'provider: credentials missing'}).waitFor();
 assert.equal(JSON.parse(await run.locator('.result').textContent()).pr_number, 1);
 assert.equal(await run.locator('.run-pr a').getAttribute('href'), pr);
 await run.getByText(JSON.parse(data.attempts.find(a=>a.CardID===firstID).TaskJSON).text.split('\nBranch: ')[1], {exact: true}).waitFor();
 const runID = await run.getAttribute('id');
 await run.evaluate(el => el.smokeIdentity = 'stable-history');
 const runsStable = await stableDocument();
 await bridge('/stop'); started = false;
 await waitActivity(d => d.agents.length === 0, 'history patch agent removal');
 await page.locator('#agent-status').getByText('No agents online').waitFor();
 assert.deepEqual(await page.locator('#' + runID).evaluate(el => ({open: el.open, stable: el.smokeIdentity})), {open: true, stable: 'stable-history'});
 await runsStable();
 for (const [state, count] of [['blocked', 1], ['active', 0], ['failed', 0], ['ready', 0], ['all', 1]]) {
  await page.locator('.tabs').getByRole('link', {name: new RegExp(`^${state}`, 'i')}).click();
  await expect(page.locator('details.run')).toHaveCount(count);
  assert.equal(new URL(page.url()).searchParams.get('card'), firstID);
 }
 await page.getByTitle('Clear filter').click();
 await expect(page.locator('details.run')).toHaveCount(3);
 // Retry creates a new immutable attempt with a new branch; no paid calls.
 await screen(boardPath, 'board');
 await bridge('/start'); started = true;
 await waitActivity(d => d.agents.some(w => w.identity.agent === agent && w.state === 'ready'), 'retry agent restart');
 const original = (await activity()).attempts.filter(a => a.CardID === secondID);
 await action('Second smoke card', 'Retry attempt');
 await waitActivity(d => d.attempts.some(a => a.CardID === secondID && a.Number === 2 && a.State === 'running'), 'retry running');
 await guards('Second smoke card');
 const retried = await waitActivity(d => d.attempts.some(a => a.CardID === secondID && a.Number === 2 && a.Result && a.Error === 'provider: credentials missing'), 'retry blocker');
 assert.deepEqual(immutableHistory(retried.attempts.filter(a => a.CardID === secondID && a.Number === 1)), immutableHistory(original));
 assert.ok(JSON.parse(retried.attempts.find(a => a.CardID === secondID && a.Number === 2).TaskJSON).text.split('\nBranch: ')[1].endsWith(`-${secondID.slice(0,6)}-a2`));
 await screen(`/runs?card=${secondID}`, 'runs');
 await expect(page.locator('details.run')).toHaveCount(2);
 for (const row of await page.locator('details.run').all()) {
  await row.locator('summary').click();
  await row.locator('.result').waitFor({state: 'visible'});
 }
 // Reset retains history; Delete requires explicit confirmation.
 await screen(boardPath, 'board');
 const recoveryStable = await stableDocument();
 const legacyHistory = (await activity()).attempts.filter(a => a.CardID === 'smoke-legacy');
 await action('Legacy Building card', 'Reset to Todo');
 await laneCard('Todo', 'Legacy Building card').waitFor();
 assert.deepEqual(immutableHistory((await activity()).attempts.filter(a => a.CardID === 'smoke-legacy')), immutableHistory(legacyHistory));
 await action('Legacy Building card', 'Delete…');
 await expect(page.locator('#delete-dialog')).toBeVisible();
 assert.equal(await card('Legacy Building card').count(), 1);
 await page.getByRole('button', {name: 'Keep card', exact: true}).click();
 assert.equal(await card('Legacy Building card').count(), 1);
 await action('Legacy Building card', 'Delete…');
 await page.getByRole('button', {name: 'Delete permanently', exact: true}).click();
 await expect(card('Legacy Building card')).toHaveCount(0);
 assert.equal((await activity()).attempts.length, 3);
 await recoveryStable();
 // Two bookmarkable projects must stay isolated in initial HTML and SSE roots.
 const otherID = await createProject('Other project');
 await createCard('Other project card');
 await expect(page.locator('.task-card')).toHaveCount(1);
 await screen(boardPath, 'board');
 await expect(page.locator('.task-card')).toHaveCount(2);
 const filteredStable = await stableDocument();
 await bridge('/stop'); started = false;
 await waitActivity(d => d.agents.length === 0, 'filtered project outage');
 await page.locator('#agent-status').getByText('No agents online').waitFor();
 await expect(card('Other project card')).toHaveCount(0);
 const frames = await page.evaluate(() => window.smokeSSE.frames);
 assert.ok(frames.includes(`project="${projectID}"`));
 assert.equal(new URL(await page.evaluate(() => window.smokeSSE.url)).searchParams.get('project'), projectID);
 assert.ok(!frames.includes('Other project card'));
 await filteredStable();
 await screen(`/board?project=${otherID}`, 'board');
 await expect(page.locator('.task-card')).toHaveCount(1);
 await expect(card('First smoke card')).toHaveCount(0);
 // Modal positioning is explicit: Tailwind's reset must not put dialogs at (0,0).
 await screen(boardPath, 'board');
 for (const viewport of [{width:1440,height:1000},{width:1200,height:720},{width:390,height:844}]) {
  await page.setViewportSize(viewport);
  for (const id of ['project-dialog','card-dialog','delete-dialog']) {
   await page.evaluate(id => document.getElementById(id).showModal(), id);
   await page.waitForTimeout(200); // Let the entrance animation finish before measuring.
   const bounds = await page.locator(`#${id}`).evaluate(el => {
    const r=el.getBoundingClientRect();
    return {cx:r.x+r.width/2,cy:r.y+r.height/2,vw:innerWidth,vh:innerHeight,left:r.left,top:r.top,right:r.right,bottom:r.bottom};
   });
   assert.ok(Math.abs(bounds.cx-bounds.vw/2)<2, `${id}: not horizontally centered`);
   assert.ok(Math.abs(bounds.cy-bounds.vh/2)<2, `${id}: not vertically centered`);
   assert.ok(bounds.left>=0 && bounds.top>=0 && bounds.right<=bounds.vw && bounds.bottom<=bounds.vh, `${id}: outside viewport`);
   await page.evaluate(id => document.getElementById(id).close(), id);
  }
 }
 await page.setViewportSize({width:1440,height:1000});
 // Capture every separated screen at both breakpoints, reject document overflow.
 const screenshot = process.env.JAWA_SMOKE_SCREENSHOT || '/tmp/jawa-factory-smoke.png';
 await mkdir(dirname(screenshot), {recursive: true});
 const screenshots = [];
 await bridge('/start'); started = true;
 await waitActivity(d => d.agents.some(w => w.identity.agent === agent && w.state === 'ready'), 'screenshot agent presence');
 for (const [view, path] of [['board', boardPath], ['runs', '/runs'], ['agents', '/agents'], ['settings', '/settings']]) {
  await screen(path, view);
  if (view === 'runs') await page.locator('details.run').first().locator('summary').click();
  for (const [size, viewport] of [['desktop', {width: 1440, height: 1000}], ['mobile', {width: 390, height: 844}]]) {
   await page.setViewportSize(viewport);
   await page.evaluate(() => document.fonts.ready);
   assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `${view}/${size}: horizontal document overflow`);
   const file = screenshot.replace(/\.png$/, `-${view}-${size}.png`);
   await page.screenshot({path: file, fullPage: true});
   screenshots.push(file);
  }
  await page.setViewportSize({width: 1440, height: 1000});
 }
 console.log('Screenshots:', screenshots.join(', '));
 await screen('/agents', 'agents');
 await bridge('/stop'); started = false;
 await waitActivity(d => d.agents.length === 0, 'final graceful outage');
 await page.locator('#agents-content').getByText('No agents online', {exact: true}).waitFor();
 assert.equal((await activity()).attempts.length, 3, 'history survives agent outage');
 await page.getByRole('button', {name: 'Sign out'}).click();
 assert.equal((await page.request.get(base + '/activity', {maxRedirects: 0})).status(), 303);
 await page.getByLabel('Username', {exact: true}).fill('smoke-admin');
 await page.getByLabel('Password', {exact: true}).fill('smoke-test-password-123');
 await page.getByRole('button', {name: 'Sign in', exact: true}).click();
 await page.locator('#board-content').waitFor();
 await screen(boardPath, 'board');
 assert.equal(await page.locator('.task-list[data-status="Building"] .task-card').count(), 2);
 await screen('/runs', 'runs');
 await expect(page.locator('details.run')).toHaveCount(3);
 assert.deepEqual(browserActivityRequests, [], 'browser must never poll /activity including re-login');
 assert.deepEqual(errors, [], 'no browser errors');
 console.log('PASS: four separated Rocket views; setup/rotation; real BONNIE presence and inspection; menu/pointer execution and retry; provider blocker; Done rollback; shortcuts/typing guards; SSE project roots, shell/drafts/keyed history preservation; state/card filters; reset/history/delete confirmation; eight overflow-checked screenshots; shutdown and re-login. No paid/external provider calls.');
} finally {
 if (started) await bridge('/stop').catch(() => {});
 await browser.close();
}
