import {launchBrowser} from './browser.mjs';
import {chooseSelect} from './select.mjs';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,mkdir} from 'node:fs/promises';
const html=await readFile(process.env.JAWA_ASSIGNMENT_HTML);
const requests=[];let reject=true;
const server=createServer(async(req,res)=>{
 const path=new URL(req.url,'http://localhost').pathname;
 if(path.startsWith('/assets/')) {
  const body=await readFile(new URL(`..${path}`,import.meta.url));
  res.writeHead(200,{'Content-Type':path.endsWith('.js')?'text/javascript':path.endsWith('.css')?'text/css':'application/octet-stream'}).end(body);
 } else if(path==='/agents/choices') res.writeHead(200,{'Content-Type':'application/json'}).end(JSON.stringify({agents:[{id:'busy-agent',provider:'fixture',model:'offline',availability:'Busy / not ready',selectable:true}]}));
 else if(req.method==='POST') {
  let body='';for await(const chunk of req)body+=chunk;
  requests.push(Object.fromEntries(new URLSearchParams(body)));
  res.writeHead(200,{'Content-Type':'text/event-stream'}).end(reject?'event: datastar-patch-elements\ndata: elements <div id="notice">Fixture rejection</div>\n\n':'event: datastar-patch-signals\ndata: signals {"assignOpen":false,"assignCard":"","assignMode":"","assignMessage":""}\n\n');
 } else if(path==='/events') res.writeHead(200,{'Content-Type':'text/event-stream'}).end();
 else res.writeHead(200,{'Content-Type':'text/html'}).end(html);
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
let browser;
try {
 browser=await launchBrowser();
 const page=await browser.newPage();page.setDefaultTimeout(5000);
 const errors=[];page.on('pageerror',e=>errors.push(e.message));
 const origin=`http://127.0.0.1:${server.address().port}`;
 const open=async(id,label)=>{
  await page.locator(`[data-card-id="${id}"] button[aria-haspopup="menu"]`).click();
  await page.locator(`#menu-${id}`).getByRole('menuitem',{name:label,exact:true}).click();
  await page.locator('#assignment-dialog').waitFor({state:'visible'});
  await page.waitForFunction(()=>!document.querySelector('#assignment-agent').disabled);
 };
 for(const width of [320,390,768,1440]) {
  await page.setViewportSize({width,height:900});await page.goto(origin);
  await open('todo','Build with agent');
  await chooseSelect(page,'#assignment-agent','busy-agent');
  assert.equal(await page.locator('#assignment-dialog [data-agent-submit]').isEnabled(),true);
  assert.equal(await page.locator('#assignment-dialog [name="acknowledge"]:enabled').count(),0);
  const box=await page.locator('#assignment-dialog').boundingBox();assert.ok(box.x>=0 && box.x+box.width<=width+1,`dialog overflow ${width}`);
  await page.keyboard.press('Escape');
  await page.waitForFunction(()=>!document.querySelector('#assignment-dialog').open);
  assert.equal(await page.locator('[data-card-id="todo"] button[aria-haspopup="menu"]').evaluate(el=>document.activeElement===el),true);
 }
 await open('planning','Build with agent');
 assert.match(await page.locator('#assignment-dialog').textContent(),/Reviewed specification/);
 await chooseSelect(page,'#assignment-agent','busy-agent');
 const submit=page.locator('#assignment-dialog [data-agent-submit]');
 assert.equal(await submit.isDisabled(),true);
 // Real shadcn control must become keyboard-operable when its plan is selected.
 await page.locator('#assignment-dialog [role=checkbox]').focus();await page.keyboard.press('Space');
 assert.equal(await submit.isEnabled(),true);
 await submit.click();await page.waitForFunction(()=>document.querySelector('#notice').textContent.includes('Fixture rejection'));
 assert.equal(await page.locator('#assignment-dialog').isVisible(),true);
 assert.equal(await page.locator('#assignment-dialog [name="target_agent_id"]').inputValue(),'busy-agent');
 assert.equal(await page.locator('#assignment-dialog [name="acknowledge"]').isChecked(),true);
 assert.equal(requests[0].expected_revision,'2');assert.equal(requests[0].acknowledge,'yes');
 reject=false;await submit.click();await page.waitForFunction(()=>!document.querySelector('#assignment-dialog').open);
 await open('ready','Fix with agent');
 assert.equal(await page.locator('#assignment-dialog [name="target_agent_id"]').inputValue(),'');
 await chooseSelect(page,'#assignment-agent','busy-agent');
 await page.locator('#assignment-message').fill('Address review feedback');
 if(process.env.JAWA_ASSIGNMENT_SCREENSHOTS) {
  await mkdir(process.env.JAWA_ASSIGNMENT_SCREENSHOTS,{recursive:true});
  await page.screenshot({path:`${process.env.JAWA_ASSIGNMENT_SCREENSHOTS}/fix-1440.png`});
 }
 await submit.click();await page.waitForFunction(()=>!document.querySelector('#assignment-dialog').open);
 assert.equal(requests[2].mode,'fix');assert.equal(requests[2].message,'Address review feedback');assert.ok(!requests[2].expected_revision);
 assert.deepEqual(errors,[]);
 console.log('Rendered assignment menus/dialogs, responsive geometry, real checkbox, rejection drafts and success resets passed');
} finally {await browser?.close();await new Promise(resolve=>server.close(resolve));}
