import {launchBrowser} from './browser.mjs';
import {createServer} from 'node:http';
import {readFile, mkdir} from 'node:fs/promises';
import assert from 'node:assert/strict';
const cards = process.env.JAWA_CARD_VISUAL_HTML;
const components = process.env.JAWA_COMPONENT_FIXTURE;
assert.ok(cards && components, 'Export card and component/Agents fixtures');
const frame = html => `event: datastar-patch-elements\ndata: elements ${html}\n\n`;
let pending;
let succeed=false;
const server = createServer(async (req,res) => {
  const path = new URL(req.url,'http://localhost').pathname;
  if (req.method === 'POST') {
    if(path==='/cards/move'){for await(const chunk of req){} res.setHeader('Content-Type','text/event-stream');res.end();return;}
    let body=''; for await (const chunk of req) body+=chunk;
    const form=new URLSearchParams(body);
    res.setHeader('Content-Type','text/event-stream');
    const name=path==='/projects'?'project':({'/cards/delete':'delete','/cards/fix':'fix','/cards/guide':'guide','/cards/cancel':'cancel','/cards/force-ready':'ready','/cards/force-stop':'force','/cards':'card'})[path];
    if (succeed) { succeed=false;res.end(frame(`<div id="${name}-error-${form.get('dialog_operation')}" class="notice dialog-error" hidden data-succeeded></div>`)+`event: datastar-patch-signals\ndata: signals {"${name}Open":false}\n\n`);return; }
    const finish=() => res.end(frame(`<div id="${name}-error-${form.get('dialog_operation')}" class="notice dialog-error" role="alert">cancel planning before resetting or deleting</div>`));
    if (form.get('card_id')==='late') pending=finish; else finish();
    return;
  }
  if (path==='/snapshot') { res.setHeader('Content-Type','text/event-stream'); res.end(frame(await readFile(`${components}.agents-patch`,'utf8'))); return; }
  if (path==='/events') { res.setHeader('Content-Type','text/event-stream');res.end();return; }
  if(path==='/sw.js') {res.writeHead(404).end();return;}
  let file=path.startsWith('/assets/') ? new URL(`../${path.slice(1)}`,import.meta.url) :
    path==='/board'?cards:path==='/archive'?`${cards}.archive`:path==='/planning'?`${components}.planning`:path==='/agents'?`${components}.agents`:components;
  try { res.setHeader('Content-Type',path.endsWith('.js')?'text/javascript':path.endsWith('.css')?'text/css':'text/html');res.end(await readFile(file)); } catch {res.writeHead(404).end();}
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await launchBrowser();
const base=`http://127.0.0.1:${server.address().port}`;
const signal = (page, values) => page.evaluate(values=>{const el=document.createElement('span');el.setAttribute('data-signals',JSON.stringify(values));document.body.append(el);},values);
const screenshots=process.env.JAWA_UI_SCREENSHOTS;
if(screenshots) await mkdir(screenshots,{recursive:true});
try {
 for(const width of [320,390,768,1024,1440]) {
  const page=await browser.newPage({viewport:{width,height:1000}});page.setDefaultTimeout(5000);
  for(const view of ['board','archive']) {
   await page.goto(`${base}/${view}`);
   const search=page.locator('.search');
   const geometry=await search.evaluate(el=>{const box=el.getBoundingClientRect();return [...el.children].filter(c=>getComputedStyle(c).display!=='none').map(c=>{const r=c.getBoundingClientRect();return {inside:r.top>=box.top&&r.bottom<=box.bottom,center:r.top+r.height/2};});});
   assert.ok(geometry.every(g=>g.inside),`Search children inside border ${width}: ${JSON.stringify(geometry)}`);
   assert.ok(Math.max(...geometry.map(g=>g.center))-Math.min(...geometry.map(g=>g.center))<2,'Search centers align');
   await page.keyboard.press('/');await page.waitForFunction(()=>document.activeElement.id==='card-filter');
   await page.getByRole('textbox',{name:'Filter cards'}).fill('no fixture matches this');
   await page.waitForFunction(()=>[...document.querySelectorAll('.task-card')].every(c=>getComputedStyle(c).display==='none'));
   await page.locator('#card-filter').fill('');
   if(view==='board') {
    const posted=page.waitForRequest(req=>new URL(req.url()).pathname==='/cards/move'&&req.method()==='POST');
    await page.locator('#move-form').evaluate(el=>{el.elements.card_id.value='todo';el.elements.status.value='Planning';el.elements.position.value='0';el.requestSubmit();});
    await posted;
    const branch=page.locator('.meta-unit');const b=await branch.boundingBox();
    assert.ok(b.height<30,'Branch icon/text stay in one unit');
    const mark=await page.locator('.view-title .proj-mark').boundingBox();
    const title=await page.locator('.view-title h1').boundingBox();
    const meta=await page.locator('.view-head .meta').boundingBox();
    assert.ok(mark.x+mark.width<=title.x,'Project mark precedes title');
    assert.ok(Math.abs(mark.y+mark.height/2-title.y-title.height/2)<2,'Project mark/title centers align');
    assert.ok(meta.y>=Math.max(mark.y+mark.height,title.y+title.height),'Git metadata is below the entire title row');
    assert.ok(Math.abs(meta.x-mark.x)<2,'Git metadata starts under the title row');
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
   }
   if(screenshots&&[390,1440].includes(width)) await page.screenshot({path:`${screenshots}/${view}-${width}.png`,fullPage:true});
  }
  await page.goto(`${base}/planning`);
  if(width>=768) {
   assert.equal((await page.locator('[aria-label="Latest run"] [title="Assigned agent"]').textContent()).trim(),'fixture-agent');
   const positions=await page.locator('[aria-label="Latest run"] .card-meta > *').evaluateAll(els=>els.map(el=>{const r=el.getBoundingClientRect();return r.top+r.height/2;}));
   assert.ok(positions.length>=3); assert.ok(Math.max(...positions)-Math.min(...positions)<3,'Latest run row');
  }
  // Foreground and background contrast measured in the actual component states.
  const primary=page.getByRole('button',{name:'Approve & build',exact:true});
  assert.equal(await primary.count(),1);
  await page.locator('#planning-acknowledge').locator('xpath=preceding-sibling::span[1]').click();
  assert.equal(await primary.isEnabled(),true);
  {
   for(const state of ['normal','hover','focus']) {
    if(state==='hover')await primary.hover(); if(state==='focus')await primary.focus();
    const colors=await primary.evaluate(el=>{const s=getComputedStyle(el);const ctx=document.createElement('canvas').getContext('2d');const rgb=c=>{ctx.fillStyle=c;ctx.fillRect(0,0,1,1);return [...ctx.getImageData(0,0,1,1).data].slice(0,3);};return [rgb(s.color),rgb(s.backgroundColor)];});
    const lum=rgb=>rgb.map(v=>{v/=255;return v<=.04045?v/12.92:((v+.055)/1.055)**2.4;}).reduce((n,v,i)=>n+v*[.2126,.7152,.0722][i],0);
    const [a,b]=colors.map(lum);assert.ok((Math.max(a,b)+.05)/(Math.min(a,b)+.05)>=4.5,`${state} primary contrast: ${colors}`);
   }
  }
  await page.evaluate(()=>window.scrollTo(0,0));
  if(screenshots&&[390,1440].includes(width))await page.screenshot({path:`${screenshots}/planning-${width}.png`,fullPage:true});
  await page.goto(`${base}/agents`);
  assert.match(await page.locator('#agent-fixture').textContent(),/custom.*org\/model/s);
  const time=page.locator('.agent-run jawa-time');
  assert.equal(await time.evaluate(el=>getComputedStyle(el).whiteSpace),'nowrap');
  const t=await time.boundingBox(), link=await page.locator('.agent-run a').boundingBox();assert.ok(link.x+link.width<=t.x,'Title yields to timestamp');
  if(screenshots&&[390,1440].includes(width))await page.screenshot({path:`${screenshots}/agents-original-${width}.png`,fullPage:true});
  await page.evaluate(()=>{const el=document.createElement('button');el.setAttribute('data-on:click',"@get('/snapshot')");el.id='patch-agents';el.textContent='Patch agents';document.body.append(el);});
  await page.locator('#patch-agents').click();await page.waitForFunction(()=>document.querySelector('#agent-fixture').textContent.includes('new/slashed/model'));await page.locator('#patch-agents').evaluate(el=>el.remove());
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),true);
  if(screenshots&&[390,1440].includes(width))await page.screenshot({path:`${screenshots}/agents-${width}.png`,fullPage:true});
  await page.goto(base);
  for(const name of ['delete','fix','guide','cancel','force','ready','card','project']) {
   await signal(page,{[`${name}Open`]:true,deleteOrphan:false,fixMessage:'Keep my draft',guideMessage:'Keep guidance',readyPR:'https://github.com/example/fixture/pull/1',readyAcknowledged:true,forceAcknowledged:true});
   const dialog=page.locator(`#${name}-dialog`);await dialog.waitFor({state:'visible'});
   if(name==='fix')await page.locator('#fix-message').fill('Keep my draft');
   if(name==='guide')await page.locator('#guide-message').fill('Keep guidance');
   // Native validation is independently covered by components.mjs.
   await dialog.locator('form').evaluate(el=>{el.noValidate=true;el.requestSubmit();});
   await dialog.locator('.dialog-error').waitFor({state:'visible'});
   assert.equal(await dialog.locator('.dialog-error').getAttribute('role'),'alert');
   assert.equal(await dialog.evaluate(el=>el.open),true);
   if(name==='fix')assert.equal(await page.locator('#fix-message').inputValue(),'Keep my draft');
   // Unsolicited Agents content morphs must not touch any shell-owned error.
   await page.evaluate(()=>{const el=document.createElement('span');el.setAttribute('data-init',"@get('/snapshot')");document.body.append(el);});
   await page.waitForTimeout(50);assert.equal(await dialog.locator('.dialog-error').isVisible(),true);
   await dialog.evaluate(async el=>{await Promise.all(el.getAnimations().map(animation=>animation.finished));});
   const alertBox=await dialog.locator('.dialog-error').boundingBox();
   const titleBox=await dialog.locator('[data-slot="dialog-title"]').boundingBox();
   assert.ok(titleBox.y-alertBox.y-alertBox.height>=12,'Dialog alert has breathing room before its title');
   if(screenshots&&[390,1440].includes(width))await page.screenshot({path:`${screenshots}/${name}-error-${width}.png`});
   await page.keyboard.press('Escape');await dialog.waitFor({state:'hidden'});await page.waitForTimeout(50);
   await signal(page,{[`${name}Open`]:true});await dialog.waitFor({state:'visible'});assert.equal(await dialog.locator('.dialog-error').isVisible(),false);
   if(name==='fix') {
    await page.locator('#fix-message').fill('Success draft');succeed=true;
    await dialog.locator('form').evaluate(el=>el.requestSubmit());await dialog.waitFor({state:'hidden'});
    assert.equal(await page.locator('#fix-message').inputValue(),'');
   } else {await signal(page,{[`${name}Open`]:false});await dialog.waitFor({state:'hidden'});}
  }
  // Dismiss/reopen while the prior response is pending. Abort plus keyed roots
  // prevent old errors (and close/reset signals) from contaminating the new card.
  await signal(page,{deleteOpen:true,deleteCard:'late'});await page.locator('#delete-dialog').waitFor({state:'visible'});
  await page.locator('#delete-dialog form').evaluate(el=>el.requestSubmit());
  await page.waitForTimeout(100);
  await signal(page,{deleteOpen:false});await page.locator('#delete-dialog').waitFor({state:'hidden'});
  await signal(page,{deleteOpen:true,deleteCard:'different'});await page.locator('#delete-dialog').waitFor({state:'visible'});
  pending?.();pending=undefined;await page.waitForTimeout(100);
  assert.equal(await page.locator('#delete-dialog .dialog-error').isVisible(),false);
  await page.close();
 }
 console.log('Compact rows, search, contrast, live agent labels and dialog error isolation passed at five widths');
} finally {await browser.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}
