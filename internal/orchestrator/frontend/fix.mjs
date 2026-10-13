import { launchBrowser } from './browser.mjs';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile} from 'node:fs/promises';

// Exercise the real bundled Rocket/Sortable/Datastar behavior without a worker.
const fixture = `<!doctype html><html><head><style>
.task-list {width:260px; min-height:240px; padding:12px; background:#ddd}
.lanes {display:flex; gap:24px} .task-card {padding:20px; margin:8px; background:white}
dialog {width:400px} textarea {display:block}
</style><script defer src="/app.js"></script></head><body
 data-signals="{fixOpen:false,fixCard:'',fixMessage:''}">
<div id="notice"></div><div id="board-content"><jawa-board project="p"><div class="lanes">
<div class="task-list" data-status="Done">
<div class="task-card" data-card-id="a" data-can-fix="true"><h3 class="card-title">Ready PR</h3>
<button id="manual" data-on:click="$fixCard = 'a'; $fixMessage = ''; $fixOpen = true">Send to Fix…</button>
<button id="conflict" data-on:click="$fixCard = 'a'; $fixMessage = 'Resolve merge conflicts'; $fixOpen = true">Fix merge conflicts…</button></div>
<div class="task-card" data-card-id="b" data-can-fix="false"><h3 class="card-title">Legacy Done</h3></div>
</div><div class="task-list" data-status="Fix"></div></div></jawa-board></div>
<dialog id="fix-dialog" data-effect="if ($fixOpen && !el.open) el.showModal(); else if (!$fixOpen && el.open) el.close()" data-on:close="$fixOpen = false" data-on:cancel="$fixOpen = false">
<form method="post" action="/cards/fix" data-on:submit__prevent="@post('/cards/fix', {contentType:'form'})">
<input type="hidden" name="card_id" data-bind:fix-card><input type="hidden" name="_csrf" value="test-csrf">
<label for="fix-message">Fix message (required)</label><textarea id="fix-message" name="message" required data-bind:fix-message autofocus></textarea>
<button type="button" id="keep" data-on:click="$fixOpen = false">Keep in Ready for review</button>
<button type="submit" id="send" data-attr:disabled="!$fixMessage.trim()">Send to Fix</button>
</form></dialog>
<form id="move-form" data-on:submit__prevent="@post('/cards/move', {contentType:'form'})" hidden><input name="card_id"><input name="status"><input name="position"></form>
</body></html>`;
const bundle = await readFile(new URL('../assets/app.js', import.meta.url));
const requests = [];
let reject = true;
const server = createServer(async (req, res) => {
  if (req.url === '/app.js') res.writeHead(200, {'Content-Type': 'text/javascript'}).end(bundle);
  else if (req.method === 'POST') {
    let body = '';
    for await (const part of req) body += part;
    requests.push({url: req.url, form: Object.fromEntries(new URLSearchParams(body))});
    res.writeHead(200, {'Content-Type': 'text/event-stream'}).end(reject
      ? 'event: datastar-patch-elements\ndata: elements <div id="notice">Repair unavailable</div>\n\n'
      : 'event: datastar-patch-signals\ndata: signals {"fixOpen":false,"fixCard":"","fixMessage":""}\n\n');
  } else res.writeHead(200, {'Content-Type': 'text/html'}).end(fixture);
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser;
try {
  browser = await launchBrowser();
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.goto(`http://127.0.0.1:${server.address().port}`);
  const dialog = page.locator('#fix-dialog');
  const message = page.locator('#fix-message');
  const waitClosed = () => page.waitForFunction(() => !document.querySelector('#fix-dialog').open);
  const order = () => page.locator('[data-status="Done"] .task-card').evaluateAll(cards => cards.map(c => c.dataset.cardId));
  const drag = async id => {
    const handle = await page.locator(`[data-card-id="${id}"] h3`).boundingBox();
    const target = await page.locator('[data-status="Fix"]').boundingBox();
    await page.mouse.move(handle.x + 50, handle.y + 10);
    await page.mouse.down();
    await page.mouse.move(handle.x + 70, handle.y + 30, {steps: 8});
    await page.waitForTimeout(200);
    await page.mouse.move(target.x + 70, target.y + 70, {steps: 20});
    await page.waitForTimeout(200);
    await page.mouse.up();
  };
  await page.click('#manual');
  await dialog.waitFor({state: 'visible'});
  assert.equal(await message.inputValue(), '');
  assert.equal(await page.locator('#send').isDisabled(), true);
  await message.fill('   ');
  assert.equal(await page.locator('#send').isDisabled(), true);
  await page.click('#keep');
  await waitClosed();
  assert.deepEqual(await order(), ['a', 'b']);
  await page.click('#conflict');
  await dialog.waitFor({state: 'visible'});
  assert.equal(await message.inputValue(), 'Resolve merge conflicts');
  await page.keyboard.press('Escape');
  await waitClosed();
  await drag('a');
  await dialog.waitFor({state: 'visible'});
  assert.equal(await message.inputValue(), '', 'drag uses the manual message dialog');
  assert.deepEqual(await order(), ['a', 'b'], 'drag restored original order before confirmation');
  assert.equal(requests.length, 0, 'opening/cancelling must not mutate');
  await page.click('#keep');
  await waitClosed();
  await drag('b');
  assert.equal(await dialog.isVisible(), false);
  assert.deepEqual(await order(), ['a', 'b']);
  assert.equal(requests.length, 0, 'legacy Done cannot enter Fix');
  await page.click('#manual');
  await message.fill('Address reviewer feedback');
  await page.click('#send');
  await page.waitForFunction(() => document.querySelector('#notice').textContent === 'Repair unavailable');
  assert.equal(await dialog.isVisible(), true, 'failed mutation preserves draft');
  assert.equal(await message.inputValue(), 'Address reviewer feedback');
  assert.deepEqual(requests[0], {url: '/cards/fix', form: {card_id: 'a', _csrf: 'test-csrf', message: 'Address reviewer feedback'}});
  reject = false;
  await page.click('#send');
  await waitClosed();
  assert.equal(await message.inputValue(), '');
  assert.equal(requests.length, 2);
  assert.deepEqual(errors, []);
  console.log('Fix dialog, validation, conflict prefill, drag cancellation, and SSE outcomes passed');
} finally {
  await browser?.close();
  await new Promise(resolve => server.close(resolve));
}
