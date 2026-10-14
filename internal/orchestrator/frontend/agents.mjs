import {launchBrowser} from './browser.mjs';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile} from 'node:fs/promises';

const fixture = `<!doctype html><meta charset="utf-8">
<button id="open" onclick="document.querySelector('dialog').showModal()">Build with agent</button>
<dialog aria-label="Choose an agent"><jawa-agent-selector><form>
<input name="card_id" value="todo" type="hidden"><input name="mode" value="build" type="hidden">
<label>Connected agent <select name="target_agent_id" required><option value="">Select an agent</option></select></label>
<p data-agent-status role="status"></p><button data-agent-refresh type="button">Refresh agents</button>
<textarea name="message">draft repair</textarea>
<section data-assignment-plan="planning"><h3>Revision 2</h3><p>Current reviewed spec</p><input name="expected_revision" value="2" disabled><input name="acknowledge" type="checkbox" value="yes" required disabled></section>
<button data-agent-submit disabled><span data-agent-submit-label>Assign work</span></button>
<button id="close" type="button" onclick="this.closest('dialog').close()">Cancel</button>
</form></jawa-agent-selector></dialog>
<script type="module">
import {setupAgentSelector} from '/selector.js';
window.requests=[];
window.fetch=(url,{signal})=>new Promise(resolve=>requests.push({url,signal,resolve}));
window.reply=(index,agents,status=200)=>requests[index].resolve(new Response(JSON.stringify({agents}),{status}));
setupAgentSelector(document.querySelector('jawa-agent-selector'),fn=>window.dispose=fn);
document.querySelector('form').addEventListener('submit',e=>{e.preventDefault();window.submission=Object.fromEntries(new FormData(e.target));});
window.ready=true;
</script>`;
const source = await readFile(new URL('./src/agent-selector.js', import.meta.url));
const server = createServer((req,res)=>res.writeHead(200,{'Content-Type':req.url==='/selector.js'?'text/javascript':'text/html'}).end(req.url==='/selector.js'?source:fixture));
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
let browser;
try {
 browser=await launchBrowser();
 const page=await browser.newPage();page.setDefaultTimeout(5000);
 await page.goto(`http://127.0.0.1:${server.address().port}`);
 await page.waitForFunction(()=>window.ready);
 const select=page.locator('select');const submit=page.locator('[data-agent-submit]');
 const entries=[{id:'busy',provider:'fixture',model:'<img src=x>',availability:'Busy / not ready',selectable:true},{id:'disabled',provider:'Unknown',model:'Unknown',availability:'Not ready',selectable:false}];
 const waitRequests=n=>page.waitForFunction(n=>requests.length===n,n);
 const reply=(i,agents=entries,code=200)=>page.evaluate(({i,agents,code})=>window.reply(i,agents,code),{i,agents,code});
 const loaded=()=>page.waitForFunction(()=>!document.querySelector('select').disabled);
 await page.locator('#open').focus();await page.keyboard.press('Enter');await waitRequests(1);
 assert.equal(await submit.isDisabled(),true);
 await reply(0);await loaded();await select.selectOption('busy');
 assert.equal(await submit.isEnabled(),true);assert.equal(await page.locator('dialog img').count(),0);
 await submit.click();assert.deepEqual(await page.evaluate(()=>submission),{card_id:'todo',mode:'build',target_agent_id:'busy',message:'draft repair'});
 // Refresh retains a still-connected selection, but removed choices clear it.
 await page.click('[data-agent-refresh]');await waitRequests(2);await reply(1);await loaded();assert.equal(await select.inputValue(),'busy');
 await page.click('[data-agent-refresh]');await waitRequests(3);await reply(2,[]);await loaded();assert.equal(await select.inputValue(),'');assert.equal(await submit.isDisabled(),true);
 // Closing aborts and reopening ignores a stale response even when abort is ignored.
 await page.click('[data-agent-refresh]');await waitRequests(4);await page.keyboard.press('Escape');
 assert.equal(await page.evaluate(()=>requests[3].signal.aborted),true);
 await page.evaluate(()=>{document.querySelector('[name="card_id"]').value='planning';});
 await page.click('#open');await waitRequests(5);await reply(3);assert.equal(await select.isDisabled(),true);
 await reply(4);await loaded();await select.selectOption('busy');assert.equal(await submit.isDisabled(),true);
 await page.locator('[name="acknowledge"]').check();assert.equal(await submit.isEnabled(),true);
 assert.equal(await page.locator('[data-agent-submit-label]').textContent(),'Approve & build with agent');
 await submit.click();assert.equal(await page.evaluate(()=>submission.expected_revision),'2');
 assert.equal(await page.evaluate(()=>submission.acknowledge),'yes');
 // Failed load keeps repair drafts and review acknowledgement, disallows dispatch.
 await page.click('[data-agent-refresh]');await waitRequests(6);await reply(5,[],503);
 await page.waitForFunction(()=>document.querySelector('[data-agent-status]').textContent.includes('Unable'));
 assert.equal(await submit.isDisabled(),true);assert.equal(await page.locator('textarea').inputValue(),'draft repair');assert.equal(await page.locator('[name="acknowledge"]').isChecked(),true);
 await page.evaluate(()=>window.dispose());
 console.log('Agent selector lifecycle, busy/empty/error, draft and revision tests passed');
} finally {await browser?.close();await new Promise(resolve=>server.close(resolve));}
