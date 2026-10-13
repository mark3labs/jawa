import {launchBrowser} from './browser.mjs';
import {createServer} from 'node:http';
import {readFile, mkdir} from 'node:fs/promises';
import assert from 'node:assert/strict';

const fixture = process.env.JAWA_COMPONENT_FIXTURE;
if (!fixture) throw new Error('Export TestComponentBrowserFixture and set JAWA_COMPONENT_FIXTURE');
const markup = await readFile(fixture);
const planning = await readFile(`${fixture}.planning`);
const server = createServer(async (req, res) => {
  if (req.url.startsWith('/assets/')) {
    try {
      res.setHeader('Content-Type', req.url.endsWith('.js') ? 'text/javascript' : req.url.endsWith('.css') ? 'text/css' : 'application/octet-stream');
      res.end(await readFile(new URL(`../${req.url.slice(1)}`, import.meta.url)));
    } catch { res.writeHead(404).end(); }
  } else if (req.url.startsWith('/events') || req.url.startsWith('/snapshot')) {
    res.setHeader('Content-Type', 'text/event-stream'); res.end();
  } else if (req.url === '/sw.js') res.writeHead(404).end();
  else res.setHeader('Content-Type', 'text/html'), res.end(req.url === '/planning' ? planning : markup);
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const browser = await launchBrowser();
const base = `http://127.0.0.1:${server.address().port}`;
const screenshots = process.env.JAWA_COMPONENT_SCREENSHOTS;
if (screenshots) await mkdir(screenshots, {recursive:true});
try {
  for (const width of [320,390,768,1024,1440]) {
    const page = await browser.newPage({viewport:{width,height:1000}});
    page.setDefaultTimeout(5000);
    const errors = []; page.on('pageerror', e => errors.push(e.message));
    await page.goto(base);
    const root = page.locator('#runtime-auto-resume').locator('xpath=preceding-sibling::span[1]');
    const input = page.locator('#runtime-auto-resume');
    assert.equal(await input.isChecked(),true);
    await root.focus(); await page.keyboard.press('Space');
    assert.equal(await input.isChecked(),false);
    assert.equal(await root.getAttribute('aria-checked'),'false');
    await page.locator('label[for="runtime-auto-resume"]').click();
    // Morph/replacement with the same native defaults must still have exactly
    // one handler: one click changes once, never double-toggles.
    await page.evaluate(() => {
      const panel = document.querySelector('#runtime-settings');
      panel.replaceWith(panel.cloneNode(true));
    });
    await root.click();
    assert.equal(await input.isChecked(),false);
    await root.click();
    assert.equal(await input.isChecked(),true);
    assert.equal(await input.isChecked(),true);
    const values = () => input.evaluate(el => new FormData(el.form).getAll(el.name));
    assert.deepEqual(await values(),['true','false']);
    await root.click(); assert.deepEqual(await values(),['false']);
    await input.evaluate(el => {
      const malformed=document.createElement('span'); malformed.dataset.slot='checkbox';
      el.form.prepend(malformed); el.form.reset();
    });
    await page.waitForFunction(() => document.querySelector('#runtime-auto-resume').previousElementSibling.getAttribute('aria-checked') === 'true');
    const clear = page.locator('[name="github-token-clear"]');
    await clear.locator('xpath=preceding-sibling::span[1]').click();
    assert.deepEqual(await clear.evaluate(el => new FormData(el.form).getAll(el.name)),['true']);
    if (screenshots && [390,1440].includes(width)) await page.screenshot({path:`${screenshots}/settings-${width}.png`,fullPage:true});
    // Signal-driven open/reset, native validity, mouse/label/Space agree.
    for (const [kind,signal] of [['force','forceAcknowledged'],['ready','readyAcknowledged'],['delete','deleteAcknowledged']]) {
      await page.evaluate(({kind,signal}) => {
        const opener = document.createElement('span');
        opener.setAttribute('data-signals',`{${signal}:false,${kind}Open:true,deleteOrphan:true}`);
        document.body.append(opener);
      },{kind,signal});
      const dialog = page.locator(`#${kind}-dialog`);
      await dialog.waitFor({state:'visible'});
      const ack = page.locator(`#${kind}-acknowledge`);
      const visible = ack.locator('xpath=preceding-sibling::span[1]');
      assert.equal(await ack.evaluate(el => el.required && !el.validity.valid),true);
      await visible.focus(); await page.keyboard.press('Space');
      assert.equal(await ack.isChecked(),true);
      assert.equal(await visible.getAttribute('aria-checked'),'true');
      await dialog.locator('label').filter({has:ack}).click();
      assert.equal(await ack.isChecked(),false);
      await visible.click();
      assert.deepEqual(await ack.evaluate(el => new FormData(el.form).getAll(el.name)),['yes']);
      if (screenshots && kind==='force' && [390,1440].includes(width)) await page.screenshot({path:`${screenshots}/acknowledgement-${width}.png`});
      // Server signal patches reset the acknowledgement on next open.
      await page.evaluate(({kind,signal}) => {
        const reset = document.createElement('span');
        reset.setAttribute('data-signals',`{${signal}:false,${kind}Open:false}`);
        document.body.append(reset);
      },{kind,signal});
      await dialog.waitFor({state:'hidden'});
      assert.equal(await ack.isChecked(),false);
      assert.equal(await visible.getAttribute('aria-checked'),'false');
    }
    // Conditional orphan requirement must disappear, not just hide visually.
    await page.evaluate(() => {
      const el=document.createElement('span'); el.setAttribute('data-signals','{deleteOrphan:false}'); document.body.append(el);
    });
    await page.waitForFunction(() => !document.querySelector('#delete-acknowledge').required);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),true);
    assert.deepEqual(errors,[]);
    await page.goto(`${base}/planning`);
    const approval = page.locator('#planning-acknowledge');
    const check = approval.locator('xpath=preceding-sibling::span[1]');
    assert.equal(await approval.evaluate(el=>el.required && !el.validity.valid),true);
    await page.locator('#plan-revision').selectOption('1');
    await check.focus(); await page.keyboard.press('Space');
    assert.equal(await approval.isChecked(),true);
    assert.equal(await approval.evaluate(el=>el.form.checkValidity()),true);
    await approval.locator('xpath=ancestor::label').click();
    assert.equal(await approval.isChecked(),false);
    assert.equal(await page.locator('.planning-section [data-slot="card"]').count() > 0,true);
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth <= innerWidth),true);
    if (screenshots && [390,1440].includes(width)) await page.screenshot({path:`${screenshots}/planning-${width}.png`,fullPage:true});
    await page.close();
  }
  const context = await browser.newContext({javaScriptEnabled:false,viewport:{width:390,height:1000}});
  const page = await context.newPage(); await page.goto(base);
  const input = page.locator('#runtime-auto-resume');
  assert.equal(await input.isVisible(),true);
  await input.uncheck();
  assert.deepEqual(await input.evaluate(el=>new FormData(el.form).getAll(el.name)),['false']);
  await page.locator('[name="github-token-clear"]').check();
  await context.close();
  console.log('Real component state, validity, field order, native fallback and viewport checks passed');
} finally { await browser.close(); await new Promise(resolve=>server.close(resolve)); }
