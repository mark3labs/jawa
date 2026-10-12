import {chromium} from 'playwright';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile} from 'node:fs/promises';

// Exercise the built Datastar form behavior, including pending and failed checks.
const bundle = await readFile(new URL('../assets/app.js', import.meta.url));
const form = (card, label) => `<form method="post" action="/prs/refresh" data-signals:pr-refreshing="false" data-indicator:pr-refreshing data-on:submit__prevent="@post('/prs/refresh', {contentType:'form'})">
<input name="_csrf" type="hidden" value="test-csrf"><input name="project_id" type="hidden" value="project"><input name="card_id" type="hidden" value="${card}"><input name="view" type="hidden" value="board"><input name="project" type="hidden" value="project">
<button data-attr:disabled="$prRefreshing" data-attr:aria-busy="$prRefreshing ? 'true' : 'false'">${label}</button></form>`;
const fixture = `<!doctype html><html><head><script defer src="/app.js"></script></head><body><div id="notice"></div><div id="board-content">${form('', 'Refresh ready PRs')}<div id="card" data-pr-check-state="unknown"><p>PR merge status is unknown.</p></div></div></body></html>`;
const requests = [];
let release;
let response = '<div id="card" data-pr-check-state="checking"><p>Checking PR status… (queued or in progress)</p><p>Awaiting a fresh check.</p></div>';
const server = createServer(async (req, res) => {
  if (req.url === '/app.js') return res.writeHead(200, {'Content-Type':'text/javascript'}).end(bundle);
  if (req.method !== 'POST') return res.writeHead(200, {'Content-Type':'text/html'}).end(fixture);
  let body = '';
  for await (const part of req) body += part;
  requests.push({url:req.url, form:Object.fromEntries(new URLSearchParams(body))});
  await new Promise(resolve => { release = resolve; });
  res.writeHead(200, {'Content-Type':'text/event-stream'}).end(`event: datastar-patch-elements\ndata: elements ${response.replaceAll("\n", "\ndata: elements ")}\n\n`);
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
  assert.equal(await page.locator('form button').getAttribute('aria-busy'),'true');
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
  assert.deepEqual(errors,[]);
  console.log('PR refresh browser checks passed');
} finally {
  release?.();
  await browser?.close();
  await new Promise(resolve => server.close(resolve));
}
