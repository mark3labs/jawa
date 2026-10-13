import {chromium} from 'playwright';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile} from 'node:fs/promises';

const fixture = `<!doctype html><html><head><script defer src="/app.js"></script></head><body data-signals="{unblockOpen:false,unblockCard:'',unblockToken:'',unblockPR:'',unblockReason:'',unblockPrompt:'',unblockAcknowledged:false}"><div id="notice"></div><button id="resume" data-on:click="$unblockCard='card'; $unblockToken='token'; $unblockPR=''; $unblockReason='blocked'; $unblockPrompt='finish within scope'; $unblockAcknowledged=false; $unblockOpen=true">Unblock and resume…</button><button id="ready" data-on:click="$unblockCard='card'; $unblockToken='token'; $unblockPR='https://github.com/example/repo/pull/7'; $unblockReason='CI blocked'; $unblockAcknowledged=false; $unblockOpen=true">Force ready…</button> <dialog id="unblock-dialog" aria-labelledby="unblock-dialog-title" data-effect="if ($unblockOpen && !el.open) el.showModal(); else if (!$unblockOpen && el.open) el.close()" data-on:close="$unblockOpen = false" data-on:cancel="$unblockOpen = false">
  <h2 id="unblock-dialog-title" data-text="$unblockPR ? 'Force ready' : 'Unblock and resume'"></h2>
  <p class="card-note" data-text="$unblockReason"></p>
  <form method="post" action="/cards/unblock" data-attr:action="$unblockPR ? '/cards/force-ready' : '/cards/unblock'" data-on:submit__prevent="@post($unblockPR ? '/cards/force-ready' : '/cards/unblock', {contentType:'form'})">
   <input type="hidden" name="_csrf" value="test-csrf"/>
   
   <input type="hidden" name="card_id" data-bind:unblock-card/>
   <input type="hidden" name="expected_unblock" data-bind:unblock-token/>
   <div class="field" data-show="!$unblockPR">
    <p>Continue the same run and branch. Completion still requires a real PR and normal verification.</p>
    <label for="unblock-prompt">Operator prompt (1–2048 UTF-8 bytes)</label>
    <textarea id="unblock-prompt" name="prompt" rows="6" maxlength="2048" data-bind:unblock-prompt data-attr:required="!$unblockPR" data-attr:disabled="!!$unblockPR" autofocus></textarea>
   </div>
   <div data-show="$unblockPR">
    <p>Attached PR: <span data-text="$unblockPR"></span></p>
    <p>Force ready bypasses CI, review, and normal readiness checks. It moves this card to Done for review, does not merge, and does not change remote provider status. A manual override is recorded.</p>
    <label><input type="checkbox" name="acknowledged" value="true" data-bind:unblock-acknowledged data-attr:required="!!$unblockPR" data-attr:disabled="!$unblockPR"/> I acknowledge this verification bypass.</label>
   </div>
   <div class="dialog-actions">
    <button type="button" class="btn" data-on:click="$unblockOpen = false">Keep blocked</button>
    <button type="submit" class="btn primary" data-attr:disabled="$unblockPR ? !$unblockAcknowledged : (!$unblockPrompt.trim() || new TextEncoder().encode($unblockPrompt.trim()).length > 2048)" data-text="$unblockPR ? 'Force ready' : 'Unblock and resume'"></button>
   </div>
  </form>
 </dialog></body></html>`;
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
      ? 'event: datastar-patch-elements\ndata: elements <div id="notice">Unblock unavailable</div>\n\n'
      : 'event: datastar-patch-signals\ndata: signals {"unblockOpen":false,"unblockCard":"","unblockPrompt":"","unblockAcknowledged":false}\n\n');
  } else res.writeHead(200, {'Content-Type': 'text/html'}).end(fixture);
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser;
try {
  browser = await chromium.launch({executablePath: process.env.CHROMIUM_PATH, headless:true,args:['--no-sandbox']});
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on('pageerror',e => errors.push(e.message));
  await page.goto(`http://127.0.0.1:${server.address().port}`);
  const dialog = page.locator('#unblock-dialog');
  const prompt = page.locator('#unblock-prompt');
  const send = dialog.locator('button[type="submit"]');
  await page.click('#resume');
  await dialog.waitFor({state:'visible'});
  assert.equal(await prompt.inputValue(),'finish within scope');
  await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('#unblock-dialog').open);
  assert.equal(requests.length,0);
  await page.click('#resume');
  await prompt.fill('  ');
  assert.equal(await send.isDisabled(),true);
  await prompt.fill('finish approved work');
  await send.click();
  await page.waitForFunction(() => document.querySelector('#notice').textContent === 'Unblock unavailable');
  assert.equal(await prompt.inputValue(),'finish approved work');
  assert.equal(await dialog.isVisible(),true);
  assert.equal(requests[0].url,'/cards/unblock');
  assert.equal(requests[0].form.expected_unblock,'token');
  reject = false;
  await send.click();
  await page.waitForFunction(() => !document.querySelector('#unblock-dialog').open);
  await page.click('#ready');
  await dialog.waitFor({state:'visible'});
  assert.equal(await send.isDisabled(),true);
  await dialog.locator('input[type="checkbox"]').check();
  await send.click();
  await page.waitForFunction(() => !document.querySelector('#unblock-dialog').open);
  assert.equal(requests.at(-1).url,'/cards/force-ready');
  assert.equal(requests.at(-1).form.acknowledged,'true');
  assert.deepEqual(errors,[]);
  console.log('Explicit unblock dialogs, draft preservation, acknowledgement and SSE reset passed');
} finally {
  await browser?.close();
  await new Promise(resolve => server.close(resolve));
}
