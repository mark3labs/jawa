import { launchBrowser } from './browser.mjs';
import {createServer} from 'node:http';
import {readFile, mkdir} from 'node:fs/promises';
import assert from 'node:assert/strict';

// Real templ markup and failure SSE exported by TestIssueRecoveryFixture.
const fixture = process.env.JAWA_ISSUE_FIXTURE;
if (!fixture) throw new Error('Export TestIssueRecoveryFixture and set JAWA_ISSUE_FIXTURE');
const markup = await readFile(fixture);
const nativeRecovery = await readFile(`${fixture}.native`);
const failure = await readFile(`${fixture}.sse`);
let posts = 0;
let submission;
const server = createServer(async (req, res) => {
  // Strip cache-busting queries (e.g. /assets/app.js?v=hash) before reading assets.
  const path = new URL(req.url, 'http://localhost').pathname;
  if (path.startsWith('/assets/')) {
    try { res.setHeader('Content-Type', path.endsWith('.js') ? 'text/javascript' : path.endsWith('.css') ? 'text/css' : 'application/octet-stream'); res.end(await readFile(new URL(`../${path.slice(1)}`, import.meta.url))); }
    catch { res.writeHead(404).end(); }
  } else if (req.url.startsWith('/projects/issues')) {
    res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify({issues:[{number:17,title:'Existing fixture issue',body:'**Markdown**',url:'https://github.com/acme/repo/issues/17'}]}));
  } else if (req.method === 'POST' && req.url === '/cards') {
    posts++; const chunks=[]; for await (const chunk of req) chunks.push(chunk);
    submission = new URLSearchParams(Buffer.concat(chunks).toString());
    res.setHeader('Content-Type', 'text/event-stream'); res.end(failure);
  } else if (req.url === '/events' || req.url === '/snapshot') {
    res.setHeader('Content-Type', 'text/event-stream'); res.end();
  } else if (req.url === '/sw.js') res.writeHead(404).end();
  else if (req.url === '/recovery-native') res.setHeader('Content-Type', 'text/html'), res.end(nativeRecovery);
  else res.setHeader('Content-Type', 'text/html'), res.end(req.url === '/native' ? markup.toString().replace('<dialog id="card-dialog"', '<dialog open id="card-dialog"') : markup);
});
await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
const browser = await launchBrowser();
const screenshotDir = process.env.JAWA_ISSUE_SCREENSHOTS;
if (screenshotDir) await mkdir(screenshotDir,{recursive:true});
try {
  const recoveryContext = await browser.newContext({javaScriptEnabled:false, viewport:{width:320,height:1000}});
  const recoveryPage = await recoveryContext.newPage();
  await recoveryPage.goto(`http://127.0.0.1:${server.address().port}/recovery-native`);
  assert.match(await recoveryPage.locator('[name="issue_submission"]').inputValue(), /^[a-f0-9]{64}$/);
  await recoveryPage.locator('[name="issue_recovery"]').selectOption('link');
  await recoveryPage.locator('[name="recovery_url"]').fill('https://github.com/acme/repo/issues/17');
  assert.ok(await recoveryPage.locator('[name="title"]').inputValue());
  assert.equal(await recoveryPage.evaluate(() => document.documentElement.scrollWidth <= innerWidth),true);
  if (screenshotDir) await recoveryPage.screenshot({path:`${screenshotDir}/native-recovery-320.png`});
  await recoveryContext.close();

  for (const width of [1440,390,320]) {
    const context = await browser.newContext({viewport:{width,height:1000},hasTouch:width<500,isMobile:width<500});
    const page = await context.newPage(); page.setDefaultTimeout(5000);
    const errors=[];page.on('pageerror',e=>errors.push(e.message));
    await page.goto(`http://127.0.0.1:${server.address().port}/board`);
    await page.locator('.topbar').getByRole('button',{name:'New card',exact:true}).click();
    await page.locator('#card-issue option').nth(1).waitFor({state:'attached'});
    const input=page.locator('#card-create-issue');
    const create=page.getByRole('checkbox',{name:'Create issue',exact:true});const autofill=page.locator('[data-issue-autofill]');
    assert.equal(await create.getAttribute('data-slot'),'checkbox');
    assert.equal(await create.isChecked(),true);
    assert.equal(await input.isChecked(),true);assert.equal(await autofill.isVisible(),false);
    await page.locator('#card-title').fill('Retained 日本語 title');
    await page.locator('#card-description').fill('  **Markdown**\nretained body  ');
    if (screenshotDir) await page.screenshot({path:`${screenshotDir}/create-${width}.png`});
    // Keyboard toggle (desktop) and tap (mobile) never submit.
    if (width===1440) { await create.focus();await page.keyboard.press('Space'); } else await create.tap();
    assert.equal(await create.isChecked(),false);
    assert.equal(await input.isChecked(),false);
    await page.locator('#card-issue').selectOption('https://github.com/acme/repo/issues/17');
    assert.equal(await page.locator('[data-create-issue]').isVisible(),false);assert.equal(await autofill.isVisible(),true);
    if (screenshotDir) await page.screenshot({path:`${screenshotDir}/linked-${width}.png`});
    await page.locator('#card-issue').selectOption('');await create.check();
    const before=posts;
    await page.getByRole('button',{name:'Create card',exact:true}).click();
    await page.locator('#card-issue-recovery[data-state="uncertain"]').waitFor();
    await page.waitForFunction(()=>!document.querySelector('#card-dialog form').hasAttribute('data-pending'));
    assert.equal(posts,before+1);assert.equal(submission.get('create_issue'),'yes');
    assert.equal(submission.get('project_id')?.length,64);assert.equal(submission.get('title'),'Retained 日本語 title');
    assert.equal(submission.get('description'),'  **Markdown**\nretained body  ');
    assert.match(submission.get('issue_submission'),/^[a-f0-9]{64}$/);
    assert.equal(await page.locator('#card-dialog').evaluate(el=>el.open),true);
    assert.equal(await page.locator('#card-title').inputValue(),'Retained 日本語 title');
    assert.equal(await create.isDisabled(),true);assert.equal(await input.isDisabled(),true);
    assert.equal(await create.getAttribute('tabindex'),'-1');
    await create.dispatchEvent('click');assert.equal(await input.isChecked(),false);
    await page.locator('[name="issue_recovery"]').selectOption('link');
    await page.locator('[name="recovery_url"]').fill('https://github.com/acme/repo/issues/17');
    const fits=await page.locator('#card-dialog').evaluate(el=>el.scrollWidth<=el.clientWidth+1);
    assert.equal(fits,true,`recovery overflow at ${width}`);
    if (screenshotDir) await page.screenshot({path:`${screenshotDir}/recovery-${width}.png`});
    await page.getByRole('button',{name:'Create card',exact:true}).click();
    await page.waitForFunction(()=>!document.querySelector('#card-dialog form').hasAttribute('data-pending'));
    assert.equal(submission.get('create_issue'),null);assert.equal(submission.get('issue_recovery'),'link');
    assert.equal(posts,before+2);assert.deepEqual(errors,[]);
    // The next draft restores both the native value and visible shadcn state.
    await page.locator('#card-dialog form').evaluate(el=>el.reset());
    await page.waitForFunction(()=>document.querySelector('[data-slot="checkbox"]').getAttribute('aria-checked')==='true');
    assert.equal(await input.isChecked(),true);assert.equal(await create.isEnabled(),true);
    assert.equal(await create.getAttribute('tabindex'),'0');
    assert.equal(await page.locator('#card-issue-recovery').textContent(),'');
    await context.close();
  }
  const nativeContext = await browser.newContext({javaScriptEnabled:false,viewport:{width:390,height:1000}});
  const nativePage = await nativeContext.newPage();nativePage.setDefaultTimeout(5000);
  // Serve an open dialog because script-disabled browsers cannot open it.
  await nativePage.goto(`http://127.0.0.1:${server.address().port}/native`);
  const nativeCreate = nativePage.locator('#card-create-issue');
  assert.equal(await nativeCreate.isVisible(),true);
  await nativeCreate.uncheck();assert.equal(await nativeCreate.isChecked(),false);
  await nativeContext.close();
  console.log('Real New card creation/recovery UI passed at 1440/390/320px; keyboard/touch and exact submit fields checked.');
} finally { await browser.close();await new Promise(resolve=>server.close(resolve)); }
