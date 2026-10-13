import { launchBrowser } from './browser.mjs';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile} from 'node:fs/promises';

// Exercise the built Datastar form behavior, including pending and failed checks.
const css = await readFile(new URL('../assets/style.css', import.meta.url));
const bundle = await readFile(new URL('../assets/app.js', import.meta.url));
const form = (card, label) => `<form class="pr-refresh-form" method="post" action="/prs/refresh" data-on:submit__prevent="@post('/prs/refresh', {contentType:'form'})">
<input name="_csrf" type="hidden" value="test-csrf"><input name="project_id" type="hidden" value="project"><input name="card_id" type="hidden" value="${card}"><input name="view" type="hidden" value="board"><input name="project" type="hidden" value="project">
<button type="submit" class="btn small" data-attr:disabled="$prRefreshing" data-attr:aria-busy="$prRefreshing ? 'true' : 'false'"><span data-show="!$prRefreshing">${label}</span><span class="pr-refresh-pending" style="display: none" data-show="$prRefreshing"><span class="pr-refresh-spinner" aria-hidden="true"></span>Refreshing ready PRs…</span></button></form>`;
const markForm = `<jawa-menu><button popovertarget="menu" aria-label="Card actions">Actions</button><div id="menu" popover="auto" role="menu"><form method="post" action="/cards/mark-merged" data-on:submit__prevent="@post('/cards/mark-merged', {contentType:'form'})"><input type="hidden" name="_csrf" value="test-csrf"><input type="hidden" name="card_id" value="card"><input type="hidden" name="expected_attempt_id" value="attempt"><input type="hidden" name="expected_pr_url" value="https://github.com/acme/repo/pull/7"><button class="menu-item" role="menuitem">Mark as merged</button></form></div></jawa-menu>`;
const mergedCard = '<div id="fixture-card" data-pr-check-state="merged"><span class="chip">Merged</span><p>Marked as merged manually</p></div>';
let manuallyMerged = false;
const fixture = `<!doctype html><html><head><meta charset="utf-8"><link rel="stylesheet" href="/style.css"><script defer src="/app.js"></script></head><body class="style-nova app"><div data-init="@get('/events')"></div><div id="notice"></div><div id="board-content">${form('', 'Refresh ready PRs')}${markForm}<div id="fixture-card" data-pr-check-state="unknown"><p>PR merge status is unknown.</p></div></div></body></html>`;
const requests = [];
let release;
let events;
let httpStatus = 200;
let disconnect = false;
let responsePatched;
let finishResponse;
const patch = (res, elements, mode = 'outer') => res.write(`event: datastar-patch-elements\ndata: mode ${mode}\ndata: elements ${elements.replaceAll("\n", "\ndata: elements ")}\n\n`);
let response = '<div id="fixture-card" data-pr-check-state="checking"><p>Checking PR status… (queued or in progress)</p><p>Awaiting a fresh check.</p></div>';
const server = createServer(async (req, res) => {
  if (req.url.endsWith('/inter.woff2')) return res.writeHead(200).end(await readFile(new URL('../assets/inter.woff2', import.meta.url)));
  if (req.url === '/style.css') return res.writeHead(200, {'Content-Type':'text/css'}).end(css);
  if (req.url.startsWith('/events')) { events = res; res.writeHead(200, {'Content-Type':'text/event-stream'}); res.flushHeaders(); return; }
  if (req.url === '/app.js') return res.writeHead(200, {'Content-Type':'text/javascript'}).end(bundle);
  if (req.method !== 'POST') return res.writeHead(200, {'Content-Type':'text/html; charset=utf-8'}).end(fixture);
  let body = '';
  for await (const part of req) body += part;
  requests.push({url:req.url, form:Object.fromEntries(new URLSearchParams(body))});
  if (req.url === '/cards/mark-merged') {
    manuallyMerged = true;
    if (req.headers['datastar-request'] !== 'true') return res.writeHead(200, {'Content-Type':'text/html; charset=utf-8'}).end(mergedCard);
  } else {
    await new Promise(resolve => { release = resolve; });
  }
  if (disconnect) { disconnect = false; res.destroy(); return; }
  const elements = manuallyMerged ? mergedCard : response;
  res.writeHead(httpStatus, {'Content-Type':'text/event-stream'});
  if (elements.startsWith('<div id="notice"')) patch(res, elements);
  else patch(res, `<div id="board-content">${form('', 'Refresh ready PRs')}${markForm}${elements}</div>`);
  if (finishResponse) { responsePatched?.(); await new Promise(resolve => { finishResponse = resolve; }); }
  res.end();
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
// Bound server-side request synchronization, including transport retries.
const waitForRequests = count => new Promise((resolve, reject) => {
  const deadline = Date.now() + 10000;
  const timer = setInterval(() => {
    if (requests.length >= count) {
      clearInterval(timer);
      resolve();
    } else if (Date.now() > deadline) {
      clearInterval(timer);
      reject(new Error(`Timed out waiting for ${count} requests`));
    }
  }, 20);
});
let browser;
try {
  browser = await launchBrowser();
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.goto(`http://127.0.0.1:${server.address().port}`);
  const button = page.locator('.pr-refresh-form button');
  const spinner = page.locator('.pr-refresh-spinner');
  const style = property => button.evaluate((el, property) => getComputedStyle(el)[property], property);
  const idle = async () => {
    await page.waitForFunction(() => document.querySelector('.pr-refresh-form button').getAttribute('aria-busy') === 'false');
    assert.equal(await button.isDisabled(), false);
    assert.equal(await style('cursor'), 'pointer');
    assert.equal(await spinner.isVisible(), false);
    assert.equal(await button.innerText(), 'Refresh ready PRs');
  };
  const pending = async () => {
    await page.waitForFunction(() => document.querySelector('.pr-refresh-form button').getAttribute('aria-busy') === 'true');
    assert.equal(await button.isDisabled(), true);
    assert.equal(await style('cursor'), 'progress');
    assert.equal(await spinner.isVisible(), true);
    assert.match(await button.innerText(), /Refreshing ready PRs…/);
  };
  await idle();
  await button.hover();
  await idle();
  await page.mouse.move(1000, 500);
  await button.focus();
  await idle();
  assert.equal(requests.length, 0, 'hover and focus never submit');
  patch(events, '<div id="notice">Unrelated live event</div>');
  await page.getByText('Unrelated live event').waitFor();
  await idle();
  await page.getByRole('button',{name:'Refresh ready PRs',exact:true}).click();
  await page.waitForFunction(() => document.querySelector('form button').disabled);
  assert.equal(await page.locator('form[action="/prs/refresh"] button').getAttribute('aria-busy'),'true');
  await pending();
  await page.keyboard.press('Enter');
  await page.keyboard.press('Space');
  await button.dispatchEvent('click');
  assert.equal(requests.length, 1);
  // The shared indicator also prevents starting a board request while pending.
  assert.equal(await page.locator('.pr-refresh-form button').isDisabled(),true);
  assert.deepEqual(requests[0],{url:'/prs/refresh',form:{_csrf:'test-csrf',project_id:'project',card_id:'',view:'board',project:'project'}});
  patch(events, `<div id="board-content">${form('', 'Refresh ready PRs')}${markForm}<div id="fixture-card" data-pr-check-state="unknown">Live patch</div></div>`, 'replace');
  await page.getByText('Live patch').waitFor({state:'attached'});
  await pending();
  patch(events, `<div id="board-content">${form('', 'Refresh ready PRs')}${markForm}<div id="fixture-card" data-pr-check-state="unknown">Morphed patch</div></div>`);
  await page.getByText('Morphed patch').waitFor();
  await pending();
  const patched = new Promise(resolve => { responsePatched = resolve; });
  finishResponse = true;
  release();
  await patched;
  await pending();
  finishResponse();
  finishResponse = null;
  await page.locator('[data-pr-check-state="checking"]').waitFor({state:'attached'});
  await page.waitForFunction(() => !document.querySelector('form button').disabled);
  assert.match(await page.locator('#fixture-card').innerText(),/Awaiting a fresh check/);
  await idle();
  await page.setViewportSize({width:390,height:844});
  response = '<div id="notice" role="alert">PR checks are unavailable. Try again.</div>';
  await button.focus();
  await page.keyboard.press('Enter');
  await pending();
  await page.emulateMedia({reducedMotion:'reduce'});
  assert.equal(await spinner.evaluate(el => getComputedStyle(el).animationName), 'none');
  const bounds = await button.boundingBox();
  assert.ok(bounds.x >= 0 && bounds.x + bounds.width <= 390);
  assert.equal(requests[1].form.card_id,'');
  release();
  await page.getByRole('alert').waitFor();
  await page.waitForFunction(() => !document.querySelector('form button').disabled);
  assert.equal(await page.locator('#fixture-card').getAttribute('data-pr-check-state'),'checking','failure must not imply clear');
  await idle();
  for (const [status, message] of [[200, 'already running'], [200, 'unavailable'], [503, 'HTTP failure']]) {
    httpStatus = status;
    response = `<div id="notice" role="alert">${message}</div>`;
    await button.click();
    await pending();
    release();
    if (status === 200) await page.getByRole('alert').filter({hasText:message}).waitFor();
    await idle();
  }
  httpStatus = 200;
  // A dropped transport retries using Datastar's existing policy, then finishes.
  const beforeDisconnect = requests.length;
  disconnect = true;
  await button.click();
  await pending();
  release();
  await waitForRequests(beforeDisconnect + 2);
  await pending();
  assert.equal(requests.length, beforeDisconnect + 2);
  release();
  await idle();
  await button.evaluate(el => { el.setAttribute('data-attr:disabled', 'true'); });
  await page.waitForFunction(() => document.querySelector('.pr-refresh-form button').disabled);
  assert.equal(await style('cursor'), 'not-allowed');
  assert.equal(await spinner.isVisible(), false);
  const disabledBackground = await style('backgroundColor');
  await button.hover();
  assert.equal(await style('backgroundColor'), disabledBackground);
  await button.evaluate(el => { el.setAttribute('data-attr:disabled', '$prRefreshing'); });
  await idle();
  // Keyboard activation uses the same native menu form as production.
  await page.getByRole('button', {name:'Card actions'}).focus();
  await page.keyboard.press('Enter');
  await page.getByRole('menuitem', {name:'Mark as merged'}).waitFor();
  await page.getByRole('menuitem', {name:'Mark as merged'}).focus();
  await page.keyboard.press('Enter');
  await page.locator('[data-pr-check-state="merged"]').waitFor();
  assert.equal(requests.at(-1).url, '/cards/mark-merged');
  assert.equal(requests.at(-1).form.expected_attempt_id, 'attempt');
  assert.equal(requests.at(-1).form.expected_pr_url, 'https://github.com/acme/repo/pull/7');
  // The fixture's provider remains open; refreshing retains manual provenance.
  response = '<div id="fixture-card" data-pr-check-state="clear">Open PR</div>';
  await page.getByRole('button', {name:'Refresh ready PRs', exact:true}).click();
  await page.waitForFunction(() => document.querySelector('form button').disabled);
  release();
  await page.waitForFunction(() => !document.querySelector('form button').disabled);
  assert.match(await page.locator('#fixture-card').innerText(), /Marked as merged manually/);
  assert.equal(await page.locator('#fixture-card .chip').count(), 1);
  // JavaScript-disabled clients still submit expected identity natively.
  const nativeContext = await browser.newContext({javaScriptEnabled:false});
  const nativePage = await nativeContext.newPage();
  await nativePage.goto(`http://127.0.0.1:${server.address().port}`);
  assert.equal(await nativePage.locator('.pr-refresh-pending').isVisible(), false);
  const beforeNative = requests.length;
  const nativeSubmission = nativePage.getByRole('button', {name:'Refresh ready PRs',exact:true}).click();
  await waitForRequests(beforeNative + 1);
  assert.deepEqual(requests.at(-1).form, {_csrf:'test-csrf',project_id:'project',card_id:'',view:'board',project:'project'});
  release();
  await nativeSubmission;
  await nativePage.goto(`http://127.0.0.1:${server.address().port}`);
  await nativePage.getByRole('button', {name:'Card actions'}).click();
  await nativePage.getByRole('menuitem', {name:'Mark as merged'}).click();
  await nativePage.getByText('Marked as merged manually').waitFor();
  assert.equal(requests.at(-1).form.expected_attempt_id, 'attempt');
  await nativeContext.close();
  assert.deepEqual(errors,[]);
  console.log('PR refresh browser checks passed');
} finally {
  release?.();
  if (typeof finishResponse === 'function') finishResponse();
  events?.end();
  server.closeAllConnections();
  await browser?.close();
  await new Promise(resolve => server.close(resolve));
}
