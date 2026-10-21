import {chooseSelect} from './select.mjs';
import { launchBrowser } from './browser.mjs';
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
const browser = await launchBrowser();
const context = await browser.newContext({viewport: {width: 1440, height: 1000}});
const page = await context.newPage();
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
  if (new URL(response.url).pathname.endsWith('/events')) {
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
 // UI/report state can precede channel ACK/cache completion; allow the local
 // broker to settle before cancelling the real host (still assert shutdown).
 if (path === '/stop') await page.waitForTimeout(750);
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
// Base UI portals action menus outside the card declaration.
async function menu(title) {
 await card(title).getByRole('button', {name: `Actions for ${title}`, exact: true}).click();
 await page.locator('[role=menu][data-open]').waitFor({state: 'visible'});
 return page.locator('[role=menu][data-open]');
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
 await page.waitForFunction(() => typeof window.jawaBoard === 'function');
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
 await page.getByLabel('Description', {exact: true}).fill('Offline browser orchestration test');
 await page.locator('#card-issue-status').filter({hasNotText: 'Loading issues…'}).waitFor();
 await page.getByRole('checkbox', {name: 'Create issue', exact: true}).uncheck();
 await page.getByRole('button', {name: 'Create card', exact: true}).click();
 await card(title).waitFor();
}
async function createProject(name) {
 await page.locator('#sidebar-nav').getByRole('button', {name: 'New project'}).click();
 await page.getByLabel('Name', {exact: true}).fill(name);
 await page.getByLabel('Repository name', {exact: true}).fill('example/smoke');
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
 await page.locator('#settings-content').waitFor();
 assert.equal(new URL(page.url()).pathname, '/settings');
 assert.equal(await page.getByLabel('Username', {exact: true}).inputValue(), 'worker');
 await page.getByRole('alert').filter({hasText: 'Complete NATS authentication setup'}).waitFor();
 await page.getByLabel('Username', {exact: true}).fill(process.env.JAWA_SMOKE_AGENT_USER);
 await page.getByLabel('New password').fill(process.env.JAWA_SMOKE_AGENT_PASSWORD);
 await page.getByRole('button', {name: 'Save credentials and continue'}).click();
 await page.locator('#board-content').waitFor();
 await screen('/settings', 'settings');
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
 const agentCard = page.locator('#agent-' + agent);
 await agentCard.getByText('Available', {exact: true}).waitFor();
 await agentCard.getByText('Inspect record', {exact: true}).click();
 const record = JSON.parse(await agentCard.locator('details pre').textContent());
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
 await page.locator('[data-dialog]').waitFor({state: 'detached'});
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
 await page.getByLabel('Description', {exact: true}).fill('Keep this while results arrive');
 const data = await waitActivity(d => d.attempts.length === 3 && d.attempts.every(a => a.Result && a.Error === 'provider: credentials missing'), 'real BONNIE outcomes and fail-closed provider verification');
 await card('Legacy Building card').locator('.card-note').filter({hasText: 'provider: credentials missing'}).waitFor();
 await expect(page.locator('#card-dialog')).toBeVisible();
 await expect(page.getByLabel('Title', {exact: true})).toHaveValue('Preserved draft');
 await expect(page.getByLabel('Description', {exact: true})).toHaveValue('Keep this while results arrive');
 await page.locator('#card-dialog').getByRole('button', {name: 'Cancel', exact: true}).click();
 await page.locator('[data-dialog]').waitFor({state: 'detached'});
 await sameDocument();
 for (const attempt of data.attempts) {
  assert.equal(attempt.Published, true);
  assert.equal(attempt.AgentID, agent);
  assert.ok(attempt.RunID && attempt.RemoteAttemptID && attempt.OutcomeJSON);
  assert.equal(attempt.RunState, 'completed');
  assert.equal(attempt.PRURL, pr);
  assert.equal(JSON.parse(attempt.Result).payload.pr_number, 1);
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
 const rejected = page.waitForResponse(r => new URL(r.url()).pathname === '/cards/move' && r.request().method() === 'POST');
 await drag('Second smoke card', 'Done', false);
 const rejection = await rejected;
 assert.equal(rejection.status(), 200, 'Datastar errors are finite SSE banner responses');
 // Assert the applied SSE notice below, rather than retrieving a response body
 // that Chromium may discard for the drag request.
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
 await page.getByLabel('Repository name', {exact: true}).fill('example/draft');
 await bridge('/start'); started = true;
 await waitActivity(d => d.agents.some(w => w.identity.agent === agent && w.state === 'ready'), 'restarted agent');
 await page.locator('#agent-status').getByText('1 agent online').waitFor();
 await expect(page.locator('#project-dialog')).toBeVisible();
 await expect(page.getByLabel('Name', {exact: true})).toHaveValue('Project draft survives SSE');
 await expect(page.getByLabel('Repository name', {exact: true})).toHaveValue('example/draft');
 await page.locator('#project-dialog').getByRole('button', {name: 'Cancel', exact: true}).click();
 await page.locator('[data-dialog]').waitFor({state: 'detached'});
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
 assert.equal(JSON.parse(await run.locator('.result').textContent()).payload.pr_number, 1);
 assert.equal(await run.locator('.run-pr a').getAttribute('href'), pr);
 await run.getByText(JSON.parse(data.attempts.find(a=>a.CardID===firstID).TaskJSON).text.split('\nBranch: ')[1], {exact: true}).waitFor();
 // Authenticated NATS run detail: committed snapshot, shared live SSE, draft
 // retention, queued follow-up and reduced-fidelity trace across viewport sizes.
 await run.getByRole('link', {name:'Open', exact:true}).click();
 await page.locator('#run-detail-content').waitFor();
 await page.getByRole('button', {name:'Send follow-up', exact:true}).waitFor();
 await page.locator('.run-tool').filter({hasText:'Reported completion'}).first().waitFor({state:'attached'});
 const toolActivity = page.locator('.run-tool').first();
 await toolActivity.evaluate(el => { for (let parent = el.parentElement; parent; parent = parent.parentElement) if (parent.tagName === 'DETAILS') parent.open = true; });
 await toolActivity.locator(':scope > summary').click();
 const activityID = await toolActivity.getAttribute('id');
 await page.getByLabel('Message / follow-up').fill('draft survives unsolicited updates');
 await page.waitForTimeout(1500);
 assert.equal(await page.getByLabel('Message / follow-up').inputValue(),'draft survives unsolicited updates');
 const detailURL=page.url();
 const secondViewer=await page.context().newPage();
 await secondViewer.goto(detailURL);
 await secondViewer.locator('#run-detail-content .run-tool').first().waitFor({state:'attached'});
 await page.getByLabel('Message / follow-up').fill('RUN_DETAIL_FOLLOWUP_FIXTURE');
 await page.getByRole('button', {name:'Send follow-up',exact:true}).click();
 await page.locator('.planning-markdown').filter({hasText:'Run detail follow-up response.'}).waitFor();
 await secondViewer.locator('.planning-markdown').filter({hasText:'Run detail follow-up response.'}).waitFor();
 await expect(page.getByLabel('Message / follow-up')).toHaveValue('');
 assert.ok(await page.locator(`[id="${activityID}"]`).evaluate(el=>el.open),'Tool disclosure lost on follow-up patch');
 await secondViewer.close();
 for(const width of [320,390,768,1024,1440]) {
  await page.setViewportSize({width,height:1000});
  assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth+1), `run detail overflow at ${width}`);
 }
 const detailShots=process.env.JAWA_SMOKE_SCREENSHOT;
 if(detailShots) {
  await mkdir(dirname(detailShots),{recursive:true});
  await page.screenshot({path:detailShots.replace('desktop.png','run-detail-desktop.png'),fullPage:true});
  await page.setViewportSize({width:390,height:844});
  await page.screenshot({path:detailShots.replace('desktop.png','run-detail-mobile.png'),fullPage:true});
  await page.setViewportSize({width:1440,height:1000});
 }
 await page.getByRole('link',{name:'Trace',exact:true}).click();
 await page.getByText('Conversation snapshot',{exact:true}).waitFor();
 await page.waitForFunction(()=>window.smokeSSE.url && new URL(window.smokeSSE.url).pathname.endsWith('/events') && new URL(window.smokeSSE.url).searchParams.get('tab') === 'trace');
 assert.equal(await page.locator('#run-composer').count(),0);
 const traceEntry=page.locator('[data-trace-entry] details').first();
 await traceEntry.locator(':scope > summary').click();
 const traceID=await traceEntry.getAttribute('id');
 await page.waitForTimeout(1500);
 assert.ok(await page.locator(`[id="${traceID}"]`).evaluate(el=>el.open));
 await page.getByLabel('Filter activity').fill('does-not-match-fixture');
 await expect(page.locator('.run-message:visible')).toHaveCount(0);
 await page.getByLabel('Filter activity').fill('');
 await screen(`/runs?card=${firstID}`, 'runs');
 await page.locator('details.run > summary').click();
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
 // Planning drag starts a durable queued session even with the agent offline.
 await createCard('Planning smoke card');
 const planningID = await card('Planning smoke card').getAttribute('data-card-id');
 await drag('Planning smoke card', 'Planning');
 await expect(card('Planning smoke card').getByText('Queued', {exact: true})).toBeVisible();
 // Dropping into Building opens review instead of starting unapproved work.
 await drag('Planning smoke card', 'Building', false);
 await page.locator('#planning-content').waitFor();
 assert.equal(new URL(page.url()).searchParams.get('card'), planningID);
 await page.getByRole('heading', {name: 'Original description'}).waitFor();
 assert.equal(await page.locator('#planning-approve-form').count(), 0, 'queued plans cannot be approved');
 assert.equal(await page.locator('#board-content').count(), 0);
 await page.waitForFunction(() => window.smokeSSE.frames.includes('id="planning-content"'));
 await page.getByRole('button', {name: 'Cancel planning & return to Todo', exact: true}).click();
 await page.locator('#planning-content').getByText('Cancelled', {exact: true}).waitFor();
 await page.getByRole('link', {name: 'Back to board', exact: true}).click();
 await laneCard('Todo', 'Planning smoke card').waitFor();
 await action('Planning smoke card', 'Delete…');
 await page.getByRole('button', {name: 'Delete permanently', exact: true}).click();
 await expect(card('Planning smoke card')).toHaveCount(0);
 // Exercise the full planning tool suspension cycle with the real backend ACK.
 await createCard('Resumable planning smoke');
 const resumableID = await card('Resumable planning smoke').getAttribute('data-card-id');
 await drag('Resumable planning smoke', 'Planning');
 await screen(`/planning?card=${resumableID}`, 'planning');
 await bridge('/start-planning'); started = true;
 await page.getByRole('heading', {name: 'Questions', exact: true}).waitFor();
 assert.equal(await page.getByRole('heading', {name: /Proposed plan/}).count(), 0, 'clarify must not fabricate a plan');
 assert.equal(await page.locator('#planning-approve-form').count(), 0);
 // Native planning badge -> filtered history -> pinned observation, no build actions.
 await screen(`/board?project=${otherID}`, 'board');
 await card('Resumable planning smoke').locator('.planning-badge').click();
 await page.locator('#runs-content').waitFor();
 const planningEntry = page.locator('#runs-content details[id^="run-planning-"]');
 await expect(planningEntry).toHaveCount(1);
 const planningEntryID = await planningEntry.getAttribute('id');
 await planningEntry.locator('summary').click();
 await planningEntry.getByRole('link', {name: 'Open', exact: true}).click();
 await page.locator('#run-detail-content').waitFor();
 assert.match(page.url(), /\/runs\/planning\//);
 await expect(page.locator('#run-composer')).toHaveCount(0);
 await page.getByRole('link', {name: 'Trace', exact: true}).click();
 await page.locator('#run-detail-content').waitFor();
 await page.getByRole('link', {name: 'Open card for recovery / guidance', exact: true}).click();
 await page.getByRole('heading', {name: 'Questions', exact: true}).waitFor();
 const questionToken = await page.getByRole('region', {name: 'Discussion'}).locator('input[name="expected_interaction"]').inputValue();
 assert.ok(questionToken);
 await page.getByLabel('Reply or request a revision').fill('API v2');
 await page.getByRole('button', {name: 'Send reply', exact: true}).click();
 await page.getByRole('heading', {name: 'Draft plan', exact: true}).first().waitFor();
 await page.locator('#planning-approve-form').waitFor();
 assert.notEqual(await page.getByRole('region', {name: 'Discussion'}).locator('input[name="expected_interaction"]').inputValue(), questionToken);
 await page.getByLabel('Reply or request a revision').fill('Use API v3 instead');
 await page.getByRole('button', {name: 'Send reply', exact: true}).click();
 await page.getByRole('heading', {name: 'Reviewed plan', exact: true}).first().waitFor();
 await screen(`/runs?card=${resumableID}`, 'runs');
 await expect(page.locator('#runs-content details[id^="run-planning-"]')).toHaveCount(1);
 assert.equal(await page.locator('#runs-content details[id^="run-planning-"]').getAttribute('id'), planningEntryID);
 await screen(`/planning?card=${resumableID}`, 'planning');
 await chooseSelect(page, '[id^=plan-revision-][role=combobox]', '2', true);
 await page.locator('#planning-approve-form [data-slot="checkbox"]').check();
 await page.getByRole('button', {name: 'Approve & build', exact: true}).click();
 await page.locator('#planning-content').getByText('Approved', {exact: true}).waitFor();
 await waitActivity(d => d.attempts.some(a => a.CardID === resumableID && a.PRURL && a.OutcomeJSON), 'approved plan distinct build');
 await screen(`/runs?card=${resumableID}`, 'runs');
 await expect(page.locator('#runs-content details[id^="run-planning-"]')).toHaveCount(1);
 await expect(page.locator('#runs-content details[id^="run-build-"]')).toHaveCount(1);
 assert.equal(await page.locator('#runs-content details[id^="run-planning-"]').getAttribute('id'), planningEntryID);
 await bridge('/stop'); started = false;
 await screen(`/board?project=${otherID}`, 'board');
 await action('Resumable planning smoke', 'Delete…');
 await page.getByRole('button', {name: 'Delete permanently', exact: true}).click();
 await expect(card('Resumable planning smoke')).toHaveCount(0);
 // Failed-run Resume must go through an actual UI click, production HTTP,
 // durable reservation, host control and real KIT continuation in BOTH modes.
 const snapshot = async id => {
  const response = await page.request.post(`${control}/resume-snapshot?card=${id}`, {headers: {Authorization: `Bearer ${process.env.JAWA_SMOKE_CONTROL_TOKEN}`}});
  assert.equal(response.status(), 200, await response.text());
  return response.json();
 };
 const waitSnapshot = async (id, predicate, description) => {
  const deadline = Date.now() + 25000;
  while (Date.now() < deadline) {
   const data = await snapshot(id);
   if (predicate(data)) return data;
   await page.waitForTimeout(150);
  }
  assert.fail(`Timed out waiting for ${description}`);
 };
 for (const mode of ['build', 'planning']) {
  const title = `Failed resume ${mode} browser`;
  await screen(boardPath, 'board');
  await bridge('/start-failing'); started = true;
  await waitActivity(d => d.agents.some(w => w.identity.agent === agent && w.state === 'ready'), 'failing offline host');
  await createCard(title);
  const id = await card(title).getAttribute('data-card-id');
  if (mode === 'build') await action(title, 'Start work');
  else await drag(title, 'Planning');
  const row = d => mode === 'build' ? d.attempts[0] : d.turns[0];
  const failed = await waitSnapshot(id, d => row(d)?.state === 'failed' && row(d)?.outcome_json && row(d)?.run_id, `${mode} natural runtime failure`);
  assert.equal(JSON.parse(row(failed).outcome_json).state, 'failed');
  assert.equal(failed.reports.length, 0, 'failure cannot be a terminal typed report');
  await bridge('/stop'); started = false;
  await bridge(`/seed-resume-context?card=${id}`);
  const before = await snapshot(id);
  await bridge(`/start-resume-${mode}`); started = true;
  await waitActivity(d => d.agents.some(w => w.identity.agent === agent && w.state === 'ready'), 'healthy restarted same host');
  const endpoint = mode === 'build' ? '/cards/resume' : '/cards/planning/resume';
  const responsePromise = page.waitForResponse(r => new URL(r.url()).pathname === endpoint && r.request().method() === 'POST');
  await action(title, 'Resume failed run');
  const response = await responsePromise;
  assert.equal(response.status(), 200);
  assert.match(response.request().postData(), /expected_resume/);
  const after = await waitSnapshot(id, d => d.resumes.length === 1 && d.resumes[0].status === 'completed' && d.reports.length === 1 && (mode !== 'build' || row(d).error === 'provider: credentials missing'), `${mode} durable resume and typed report`);
  const previous = row(before), current = row(after);
  for (const key of ['id','number','task_json','agent_id','run_id','remote_attempt_id', ...(mode === 'build' ? ['task_id'] : ['session_id'])]) {
   assert.deepEqual(current[key], previous[key], `${mode} changed ${key}`);
  }
  assert.equal(after.attempts.length, before.attempts.length);
  assert.equal(after.turns.length, before.turns.length);
  assert.equal(after.sessions.length, before.sessions.length);
  if (mode === 'planning') assert.equal(after.sessions[0].id, before.sessions[0].id);
  const task = JSON.parse(previous.task_json);
  assert.equal(after.prompts.length, 2, 'workspace check then typed report, no fresh dispatch');
  assert.equal(before.workspaceMarker, 'resume-workspace-marker');
  assert.equal(after.workspaceMarker, before.workspaceMarker, 'real sandbox file survives restart');
  assert.ok(after.records.some(r => JSON.stringify(r.payload || {}).includes('workspace-preserved') && r.seq > before.records.at(-1).seq), 'resumed shell actually checked saved file');
  assert.equal(after.prompts[0].split(task.text).length - 1, 1, 'original prompt exactly once');
  assert.ok(after.prompts[0].includes('browser-saved-context-marker'), 'saved context restored');
  const users = records => records.filter(r => r.kind === 'message' && r.role === 'user');
  assert.deepEqual(users(after.records).filter(r => r.text), users(before.records), 'original saved user input unchanged and not duplicated');
  const addedUsers = users(after.records).filter(r => !users(before.records).some(old => old.seq === r.seq));
  // BONNIE StartRetry journals its empty continuation input, not the task again.
  assert.equal(addedUsers.length, 1);
  assert.deepEqual(addedUsers[0].payload.content, [{type: 'text', data: {text: ''}}]);
  const bindings = records => records.filter(r => r.kind === 'sandbox');
  assert.ok(bindings(before.records).length, 'real sandbox binding exists');
  assert.deepEqual(bindings(after.records).slice(0, bindings(before.records).length), bindings(before.records));
  assert.deepEqual(bindings(after.records).at(-1).payload, bindings(before.records).at(-1).payload, 'same saved workspace binding');
  assert.equal(after.records.filter(r => r.ext_type === 'jawa_failed_resume_v1').length, 1);
  assert.equal(after.records.filter(r => r.ext_type === 'jawa_failed_resume_receipt_v1').length, 1);
  const receipt = after.records.find(r => r.ext_type === 'jawa_failed_resume_receipt_v1').payload;
  assert.equal(receipt.status, 'completed');
  assert.equal(receipt.target.run_id, previous.run_id);
  if (mode === 'build') {
   assert.equal(current.pr_url, pr);
   assert.equal(current.error, 'provider: credentials missing', 'real verifier remains fail closed');
   await card(title).getByTitle('Open pull request').waitFor();
  } else {
   assert.equal(after.sessions[0].state, 'questions');
   // Resume ran from the board menu; inspect the conversation in its detail view.
   await screen(`/planning?card=${id}`, 'planning');
   await page.getByRole('heading', {name: 'Questions', exact: true}).waitFor();
   await page.getByText('Resume browser API?', {exact: true}).first().waitFor();
   assert.equal(await page.getByRole('button', {name: 'Resume failed run', exact: true}).count(), 0);
   // Existing Answer path must still work after failed-run continuation.
   await page.getByLabel('Reply or request a revision').fill('Use the saved API');
   await page.getByRole('button', {name: 'Send reply', exact: true}).click();
   await page.getByRole('heading', {name: 'Resumed browser plan', exact: true}).first().waitFor();
   const answered = await snapshot(id);
   assert.equal(answered.turns.length, 1);
   assert.equal(answered.turns[0].run_id, previous.run_id);
   assert.equal(answered.sessions[0].revision, 1);
  }
  console.log('PASS failed-run browser Resume:', JSON.stringify({mode,card:id,task:previous.task_id || previous.id,run:previous.run_id,remoteAttempt:previous.remote_attempt_id,boardAttempt:previous.number,session:previous.session_id,command:after.resumes[0].id,http:endpoint,receipt:receipt.status,branch:task.text.split('\nBranch: ')[1],samePrompt:true,savedContext:true,sameSandbox:true}));
  await bridge('/stop'); started = false;
  if (mode === 'planning') {
   await page.getByRole('button', {name: 'Cancel planning & return to Todo', exact: true}).click();
   await page.locator('#planning-content').getByText('Cancelled', {exact: true}).waitFor();
  }
  await screen(boardPath, 'board');
  await action(title, 'Delete…');
  await page.getByRole('button', {name: 'Delete permanently', exact: true}).click();
  await expect(card(title)).toHaveCount(0);
 }
 // Modal positioning is explicit: Tailwind's reset must not put dialogs at (0,0).
 await screen(boardPath, 'board');
 for (const viewport of [{width:1440,height:1000},{width:1200,height:720},{width:390,height:844}]) {
  await page.setViewportSize(viewport);
  for (const id of ['project-dialog','card-dialog']) {
   await page.getByRole('button', {name: id === 'project-dialog' ? 'New project' : 'New card', exact: true}).first().click();
   await page.getByRole('dialog').waitFor();
   await page.waitForTimeout(200); // Let the entrance animation finish before measuring.
   const bounds = await page.locator(`#${id}`).evaluate(el => {
    const r=el.getBoundingClientRect();
    return {cx:r.x+r.width/2,cy:r.y+r.height/2,vw:innerWidth,vh:innerHeight,left:r.left,top:r.top,right:r.right,bottom:r.bottom};
   });
   assert.ok(Math.abs(bounds.cx-bounds.vw/2)<2, `${id}: not horizontally centered`);
   assert.ok(Math.abs(bounds.cy-bounds.vh/2)<2, `${id}: not vertically centered`);
   assert.ok(bounds.left>=0 && bounds.top>=0 && bounds.right<=bounds.vw && bounds.bottom<=bounds.vh, `${id}: outside viewport`);
   await page.keyboard.press('Escape');
   await page.locator('[data-dialog]').waitFor({state: 'detached'});
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
 console.log('PASS: resumable clarify/answer/proposal/revision/approval/distinct-build full cycle; planning drag/queued badge/review guard/cancel and live detail; four separated Datastar views; setup/rotation; real BONNIE presence and inspection; menu/pointer execution and retry; provider blocker; Done rollback; shortcuts/typing guards; SSE project roots, shell/drafts/keyed history preservation; state/card filters; reset/history/delete confirmation; eight overflow-checked screenshots; shutdown and re-login. No paid/external provider calls.');
} finally {
 if (started) await bridge('/stop').catch(() => {});
 await browser.close();
}
