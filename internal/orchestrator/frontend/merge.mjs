import {launchBrowser} from './browser.mjs';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile, mkdir} from 'node:fs/promises';

// Real Go-rendered page and bundled components; every provider response is local.
const fixture = await readFile(process.env.JAWA_MERGE_HTML || new URL('./node_modules/.merge.html', import.meta.url), 'utf8');
const assets = new URL('../assets/', import.meta.url);
const requests = [];
let respond;
const patch = html => `event: datastar-patch-elements\ndata: elements ${html}\n\n`;
const server = createServer(async (req,res) => {
  const name = req.url.split('/').pop().split('?')[0];
  if (['app.js','style.css','inter.woff2'].includes(name)) {
    res.writeHead(200, {'Content-Type':name.endsWith('.js') ? 'text/javascript' : name.endsWith('.css') ? 'text/css' : 'font/woff2'}).end(await readFile(new URL(name,assets)));
  } else if (req.method === 'POST') {
    let body = ''; for await (const part of req) body += part;
    const form = Object.fromEntries(new URLSearchParams(body));
    requests.push(form);
    res.writeHead(200, {'Content-Type':'text/event-stream'});res.flushHeaders();
    respond = (success=false) => {
      res.end(patch(`<div role="alert" class="notice dialog-error" id="merge-error-${form.dialog_operation}" ${success?'hidden data-succeeded':''}>${success?'':'Merge not confirmed; refresh ready PRs.'}</div>`) + (success ? patch('<div id="board-content"><p data-pr-check-state="merged">Merged</p></div>') : ''));
    };
  } else if (req.url.startsWith('/events')) {
    res.writeHead(200, {'Content-Type':'text/event-stream'}).end();
  } else res.writeHead(200, {'Content-Type':'text/html'}).end(fixture);
});
await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
const browser = await launchBrowser();
const page = await browser.newPage();
const errors = [];page.on('pageerror',e=>errors.push(e.message));
const dialog = page.locator('#merge-dialog');
const method = dialog.locator('input[name="method"]');
const open = async () => {await page.getByRole('button',{name:'Merge…',exact:true}).click();await dialog.waitFor({state:'visible'});};
const cancel = async () => {await dialog.getByRole('button',{name:'Cancel',exact:true}).click();await dialog.waitFor({state:'hidden'});};
const choose = async label => {
  await dialog.locator('[data-slot="select-trigger"]').click();
  await page.getByRole('option',{name:label,exact:true}).click();
};
const submitted = async count => {
 const end=Date.now()+5000;
 while(requests.length<count && Date.now()<end) await new Promise(r=>setTimeout(r,10));
 assert.equal(requests.length,count);
};
try {
 await page.goto(`http://127.0.0.1:${server.address().port}/board`);
 await page.getByRole('button',{name:'Merge…',exact:true}).waitFor();
 assert.equal(await page.getByRole('button',{name:'Merge…',exact:true}).count(),1);
 await open();
 assert.equal(await method.inputValue(),'merge');
 assert.match(await dialog.textContent(),/pull\/7/);
 assert.match(await dialog.textContent(),/main/);
 assert.match(await dialog.textContent(),/a{40}/);
 await choose('Squash');assert.equal(await method.inputValue(),'squash');
 await cancel();assert.equal(requests.length,0);
 await open();assert.equal(await method.inputValue(),'merge');
 await choose('Rebase');assert.equal(await method.inputValue(),'rebase');
 await page.keyboard.press('Escape');await dialog.waitFor({state:'hidden'});assert.equal(requests.length,0);
 await open();await choose('Squash');
 const submit=dialog.getByRole('button',{name:'Merge pull request',exact:true});
 await submit.click();await submitted(1);
 assert.equal(await submit.isDisabled(),true);
 assert.equal(requests[0].method,'squash');
 assert.equal(requests[0].operation,requests[0].dialog_operation);
 assert.equal(requests[0]._csrf,'fixture-csrf');
 respond();await dialog.getByRole('alert').waitFor({state:'visible'});
 assert.equal(await method.inputValue(),'squash');
 assert.equal(await dialog.isVisible(),true);
 await page.waitForTimeout(200);assert.equal(requests.length,1,'no automatic retry');
 // A response for a cancelled opening must not close or poison a new one.
 await cancel();await open();await submit.click();await submitted(2);
 const stale=respond;
 await cancel();await open();
 stale(true);await page.waitForTimeout(150);
 assert.equal(await dialog.isVisible(),true);
 assert.equal(await dialog.getByRole('alert').isVisible(),false);
 await cancel();
 // Reset fixture after the old response's independent board refresh.
 await page.reload();await open();await submit.click();await submitted(3);
 respond(true);await dialog.waitFor({state:'hidden'});
 assert.equal(await page.getByRole('button',{name:'Merge…',exact:true}).count(),0);
 assert.match(await page.locator('#board-content').textContent(),/Merged/);
 await page.reload();
 await mkdir(new URL('./node_modules/.merge-screenshots/',import.meta.url),{recursive:true});
 for(const width of [320,390,768,1440]) {
  await page.setViewportSize({width,height:900});
  await page.screenshot({path:new URL(`./node_modules/.merge-screenshots/board-${width}.png`,import.meta.url).pathname,fullPage:true});
  await open();
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true,`no horizontal overflow at ${width}`);
  await page.screenshot({path:new URL(`./node_modules/.merge-screenshots/dialog-${width}.png`,import.meta.url).pathname,fullPage:true});
  await cancel();
 }
 assert.deepEqual(errors,[]);
 console.log('merge browser regression passed (real renderer, four viewport screenshots)');
} finally {await browser.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}
