import {launchBrowser} from './browser.mjs';
import {createServer} from 'node:http';
import {readFile,mkdir} from 'node:fs/promises';
import assert from 'node:assert/strict';
const fixture=await readFile(process.env.JAWA_AUTO_MERGE_HTML);
const server=createServer(async(req,res)=>{
 if(req.url==='/app.js'||req.url==='/style.css') {
  res.setHeader('Content-Type',req.url.endsWith('.js')?'text/javascript':'text/css');
  res.end(await readFile(new URL(`../assets${req.url}`,import.meta.url)));
 } else res.setHeader('Content-Type','text/html'),res.end(fixture);
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const browser=await launchBrowser();
try {
 for(const width of [390,1280]) {
  const page=await browser.newPage({viewport:{width,height:800},hasTouch:width===390});
  const errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.goto(`http://127.0.0.1:${server.address().port}`);
  const inherit=page.locator('[data-merge-inherit]');const on=page.locator('[data-merge-on]');
  const mode=()=>page.locator('[name=auto_merge_mode]').inputValue();
  assert.equal(await mode(),'inherit');assert.equal(await on.getAttribute('aria-disabled'),'true');
  await inherit.focus();await page.keyboard.press('Space');assert.equal(await mode(),'off');
  assert.equal(await on.getAttribute('tabindex'),'0');
  await page.keyboard.press('Tab');assert.equal(await on.evaluate(el=>document.activeElement===el),true);
  await on.click();assert.equal(await mode(),'on');
  const data=await page.locator('form').evaluate(el=>Object.fromEntries(new FormData(el)));
  assert.equal(data.auto_merge_mode,'on');
  await inherit.click();assert.equal(await mode(),'inherit');
  await inherit.click();assert.equal(await mode(),'on');
  await on.click();assert.equal(await mode(),'off');
  await page.locator('[type=reset]').click();assert.equal(await mode(),'inherit');
  await inherit.click();await on.click();
  if(process.env.JAWA_AUTO_MERGE_SCREENSHOTS) {
   await mkdir(process.env.JAWA_AUTO_MERGE_SCREENSHOTS,{recursive:true});
   await page.screenshot({path:`${process.env.JAWA_AUTO_MERGE_SCREENSHOTS}/auto-merge-${width}.png`,fullPage:true});
  }
  assert.deepEqual(errors,[]);await page.close();
 }
 console.log('Auto merge switches: canonical serialization, precedence draft, keyboard/touch and reset passed');
} finally {await browser.close();await new Promise(resolve=>server.close(resolve));}
