import {chromium} from 'playwright';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile} from 'node:fs/promises';

// Exercise the built Datastar form behavior, including pending and failed checks.
const bundle = await readFile(new URL('../assets/app.js', import.meta.url));
const form = (card, label) => `<form method="post" action="/prs/refresh" data-signals:pr-refreshing="false" data-indicator:pr-refreshing data-on:submit__prevent="@post('/prs/refresh', {contentType:'form'})">
<input name="_csrf" type="hidden" value="test-csrf"><input name="project_id" type="hidden" value="project"><input name="card_id" type="hidden" value="${card}"><input name="view" type="hidden" value="board"><input name="project" type="hidden" value="project">
<button data-attr:disabled="$prRefreshing" data-attr:aria-busy="$prRefreshing ? 'true' : 'false'">${label}</button></form>`;
const markForm = `<jawa-menu><button popovertarget="menu" aria-label="Card actions">Actions</button><div id="menu" popover="auto" role="menu"><form method="post" action="/cards/mark-merged" data-on:submit__prevent="@post('/cards/mark-merged', {contentType:'form'})"><input type="hidden" name="_csrf" value="test-csrf"><input type="hidden" name="card_id" value="card"><input type="hidden" name="expected_attempt_id" value="attempt"><input type="hidden" name="expected_pr_url" value="https://github.com/acme/repo/pull/7"><button class="menu-item" role="menuitem">Mark as merged</button></form></div></jawa-menu>`;
const mergedCard = '<div id="card" data-pr-check-state="merged"><span class="chip">Merged</span><p>Marked as merged manually</p></div>';
let manuallyMerged = false;
const fixture = `<!doctype html><html><head><script defer src="/app.js"></script></head><body><div id="notice"></div><div id="board-content">${form('', 'Refresh ready PRs')}${markForm}<div id="card" data-pr-check-state="unknown"><p>PR merge status is unknown.</p></div></div></body></html>`;
const requests = [];
let release;
let response = '<div id="card" data-pr-check-state="checking"><p>Checking PR status… (queued or in progress)</p><p>Awaiting a fresh check.</p></div>';
const server = createServer(async (req, res) => {
  if (req.url === '/app.js') return res.writeHead(200, {'Content-Type':'text/javascript'}).end(bundle);
  if (req.method !== 'POST') return res.writeHead(200, {'Content-Type':'text/html'}).end(fixture);
  let body = '';
  for await (const part of req) body += part;
  requests.push({url:req.url, form:Object.fromEntries(new URLSearchParams(body))});
  if (req.url === '/cards/mark-merged') {
    manuallyMerged = true;
    if (req.headers['datastar-request'] !== 'true') return res.writeHead(200, {'Content-Type':'text/html'}).end(mergedCard);
  } else {
    await new Promise(resolve => { release = resolve; });
  }
  const elements = manuallyMerged ? mergedCard : response;
  res.writeHead(200, {'Content-Type':'text/event-stream'}).end(`event: datastar-patch-elements\ndata: elements ${elements.replaceAll("\n", "\ndata: elements ")}\n\n`);
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser;
try {
  browser = await chromium.launch({executablePath:process.env.CHROMIUM_PATH || '/etc/profiles/per-user/space_cowboy/bin/chromium',headless:true,args:['--no-sandbox']});
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.goto(`http://127.0.0.1:${server.address().port}`);
  await page.getByRole('button',{name:'Refresh ready PRs',exact:true}).click();
  await page.waitForFunction(() => document.querySelector('form button').disabled);
  assert.equal(await page.locator('form[action="/prs/refresh"] button').getAttribute('aria-busy'),'true');
  // The shared indicator also prevents starting a board request while pending.
  assert.equal(await page.getByRole('button',{name:'Refresh ready PRs',exact:true}).isDisabled(),true);
  assert.deepEqual(requests[0],{url:'/prs/refresh',form:{_csrf:'test-csrf',project_id:'project',card_id:'',view:'board',project:'project'}});
  release();
  await page.locator('[data-pr-check-state="checking"]').waitFor();
  await page.waitForFunction(() => !document.querySelector('form button').disabled);
  assert.match(await page.locator('#card').innerText(),/Awaiting a fresh check/);
  response = '<div id="notice" role="alert">PR checks are unavailable. Try again.</div>';
  await page.getByRole('button',{name:'Refresh ready PRs',exact:true}).click();
  await page.waitForFunction(() => document.querySelector('form button').disabled);
  assert.equal(requests[1].form.card_id,'');
  release();
  await page.getByRole('alert').waitFor();
  await page.waitForFunction(() => !document.querySelector('form button').disabled);
  assert.equal(await page.locator('#card').getAttribute('data-pr-check-state'),'checking','failure must not imply clear');
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
  response = '<div id="card" data-pr-check-state="clear">Open PR</div>';
  await page.getByRole('button', {name:'Refresh ready PRs', exact:true}).click();
  await page.waitForFunction(() => document.querySelector('form button').disabled);
  release();
  await page.waitForFunction(() => !document.querySelector('form button').disabled);
  assert.match(await page.locator('#card').innerText(), /Marked as merged manually/);
  assert.equal(await page.locator('#card .chip').count(), 1);
  // JavaScript-disabled clients still submit expected identity natively.
  const nativeContext = await browser.newContext({javaScriptEnabled:false});
  const nativePage = await nativeContext.newPage();
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
  await browser?.close();
  await new Promise(resolve => server.close(resolve));
}
