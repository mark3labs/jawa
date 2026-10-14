import {launchBrowser} from './browser.mjs';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile} from 'node:fs/promises';
import {execFileSync} from 'node:child_process';
import {chooseSelect} from './select.mjs';

// Real templ dialog markup and the pinned shadcn runtime; Datastar stays out so
// the selector's own lifecycle is exercised in isolation.
const fixturePath = new URL('./node_modules/.agent-selector.html', import.meta.url).pathname;
execFileSync('go', ['test', '../../../internal/orchestrator', '-run', '^TestAgentSelectorFixture$', '-count=1'], {env: {...process.env, JAWA_AGENT_SELECTOR_HTML: fixturePath}});
const dialog = await readFile(fixturePath, 'utf8');
const runtime = execFileSync('sh', ['./runtime.sh'], {cwd: new URL('.', import.meta.url).pathname, maxBuffer: 1 << 26});
const fixture = `<!doctype html><meta charset="utf-8">
<button id="open" onclick="document.querySelector('dialog').showModal()">Build with agent</button>
${dialog}
<script src="/runtime.js"></script>
<script type="module">
import {setupAgentSelector} from '/agent-selector.js';
const form = document.querySelector('#assignment-dialog form');
form.querySelector('[name="card_id"]').value = 'todo';
form.querySelector('[name="mode"]').value = 'build';
form.querySelector('[name="message"]').value = 'draft repair';
window.requests=[];
window.fetch=(url,{signal})=>new Promise(resolve=>requests.push({url,signal,resolve}));
window.reply=(index,agents,status=200)=>requests[index].resolve(new Response(JSON.stringify({agents}),{status}));
setupAgentSelector(form.querySelector('jawa-agent-selector'),fn=>window.dispose=fn);
form.addEventListener('submit',e=>{e.preventDefault();window.submission=Object.fromEntries(new FormData(e.target));});
window.ready=true;
</script>`;
const server = createServer(async (req,res)=>{
  if (req.url==='/runtime.js') return res.writeHead(200,{'Content-Type':'text/javascript'}).end(runtime);
  if (/^\/[a-z-]+\.js$/.test(req.url)) return res.writeHead(200,{'Content-Type':'text/javascript'}).end(await readFile(new URL(`./src${req.url}`, import.meta.url)));
  res.writeHead(200,{'Content-Type':'text/html'}).end(fixture);
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
let browser;
try {
 browser=await launchBrowser();
 const page=await browser.newPage();page.setDefaultTimeout(5000);
 await page.goto(`http://127.0.0.1:${server.address().port}`);
 await page.waitForFunction(()=>window.ready);
 const select=page.locator('#assignment-dialog [name="target_agent_id"]');const trigger='#assignment-agent';const submit=page.locator('[data-agent-submit]');
 const entries=[{id:'busy',provider:'fixture',model:'<img src=x>',availability:'Busy / not ready',selectable:true},{id:'disabled',provider:'Unknown',model:'Unknown',availability:'Not ready',selectable:false}];
 const waitRequests=n=>page.waitForFunction(n=>requests.length===n,n);
 const reply=(i,agents=entries,code=200)=>page.evaluate(({i,agents,code})=>window.reply(i,agents,code),{i,agents,code});
 const loaded=()=>page.waitForFunction(()=>!document.querySelector('#assignment-agent').disabled);
 const pick=value=>chooseSelect(page,trigger,value);
 await page.locator('#open').focus();await page.keyboard.press('Enter');await waitRequests(1);
 assert.equal(await submit.isDisabled(),true);
 await reply(0);await loaded();await pick('busy');
 assert.equal(await submit.isEnabled(),true);assert.equal(await page.locator('dialog img').count(),0);
 await submit.click();assert.deepEqual(await page.evaluate(()=>{const {card_id,mode,target_agent_id,message,expected_revision,acknowledge}=submission;return {card_id,mode,target_agent_id,message,expected_revision,acknowledge};}),{card_id:'todo',mode:'build',target_agent_id:'busy',message:'draft repair',expected_revision:undefined,acknowledge:undefined});
 // Non-selectable agents render as disabled options.
 assert.equal(await page.locator('#assignment-agent-content [data-templ-value="disabled"]').getAttribute('aria-disabled'),'true');
 // Refresh retains a still-connected selection, but removed choices clear it.
 await page.click('[data-agent-refresh]');await waitRequests(2);await reply(1);await loaded();assert.equal(await select.inputValue(),'busy');
 await page.click('[data-agent-refresh]');await waitRequests(3);await reply(2,[]);await loaded();assert.equal(await select.inputValue(),'');assert.equal(await submit.isDisabled(),true);
 // Closing aborts and reopening ignores a stale response even when abort is ignored.
 await page.click('[data-agent-refresh]');await waitRequests(4);await page.keyboard.press('Escape');
 assert.equal(await page.evaluate(()=>requests[3].signal.aborted),true);
 await page.evaluate(()=>{document.querySelector('[name="card_id"]').value='planning';});
 await page.click('#open');await waitRequests(5);await reply(3);assert.equal(await page.locator(trigger).isDisabled(),true);
 await reply(4);await loaded();await pick('busy');assert.equal(await submit.isDisabled(),true);
 await page.locator('#assignment-dialog [role=checkbox]').focus();await page.keyboard.press('Space');assert.equal(await submit.isEnabled(),true);
 assert.equal(await page.locator('[data-agent-submit-label]').textContent(),'Approve & build with agent');
 await submit.click();assert.equal(await page.evaluate(()=>submission.expected_revision),'2');
 assert.equal(await page.evaluate(()=>submission.acknowledge),'yes');
 // Failed load keeps repair drafts and review acknowledgement, disallows dispatch.
 await page.click('[data-agent-refresh]');await waitRequests(6);await reply(5,[],503);
 await page.waitForFunction(()=>document.querySelector('[data-agent-status]').textContent.includes('Unable'));
 assert.equal(await submit.isDisabled(),true);assert.equal(await page.locator('#assignment-message').inputValue(),'draft repair');assert.equal(await page.locator('[name="acknowledge"]').isChecked(),true);
 // A finished transport request is not mutation success and must not reset
 // drafts or accidentally enable a selector with no valid target.
 await page.evaluate(() => {
   const el = document.querySelector('form');
   document.dispatchEvent(new CustomEvent('datastar-fetch',{detail:{el,type:'started'}}));
   document.dispatchEvent(new CustomEvent('datastar-fetch',{detail:{el,type:'finished'}}));
 });
 assert.equal(await submit.isDisabled(),true);
 assert.equal(await page.locator('#assignment-message').inputValue(),'draft repair');
 await page.evaluate(()=>window.dispose());
 console.log('Agent selector lifecycle, busy/empty/error, draft and revision tests passed');
} finally {await browser?.close();await new Promise(resolve=>server.close(resolve));}
