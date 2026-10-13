import { chromium, expect } from 'playwright/test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

const fixture = process.env.JAWA_RUN_DETAIL_HTML;
assert.ok(fixture, 'Export TestRunDetailVisualFixture with JAWA_RUN_DETAIL_HTML first');
const assets = new URL('../assets/', import.meta.url);
const streams = new Set();
const server = createServer(async (req, res) => {
  const url = new URL(req.url, 'http://localhost');
  if (url.pathname.endsWith('/events')) {
    res.writeHead(200, {'Content-Type':'text/event-stream', 'Cache-Control':'no-store'});
    res.write(': fixture stream\n\n');
    streams.add(res);
    req.on('close', () => streams.delete(res));
    return;
  }
  const asset = /^\/assets\/([a-zA-Z0-9_.-]+)$/.exec(url.pathname);
  const variant = `${url.searchParams.get('offline') ? 'offline-' : ''}${url.searchParams.get('tab') === 'trace' ? 'trace' : 'chat'}`;
  const file = asset ? fileURLToPath(new URL(asset[1], assets)) : url.pathname.startsWith('/runs/') ? `${fixture}.${variant}` : null;
  if (!file) { res.writeHead(404).end(); return; }
  try {
    res.writeHead(200, {'Content-Type':asset ? url.pathname.endsWith('.css') ? 'text/css' : url.pathname.endsWith('.js') ? 'text/javascript' : 'application/octet-stream' : 'text/html'}).end(await readFile(file));
  } catch { res.writeHead(404).end(); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser;
const url = `http://127.0.0.1:${server.address().port}/runs/11111111-1111-4111-8111-111111111111`;
try {
  browser = await chromium.launch({headless:true,args:['--no-sandbox'], ...(process.env.CHROMIUM_PATH ? {executablePath:process.env.CHROMIUM_PATH} : {})});
  const context = await browser.newContext({serviceWorkers:'block'});
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  for (const width of [320,390,768,1024,1440]) {
    await page.setViewportSize({width,height:1000});
    for (const tab of ['chat','trace']) {
      for (const offline of [false,true]) {
        await page.goto(`${url}?tab=${tab}${offline ? '&offline=1' : ''}`);
        await page.locator('#run-detail-content').waitFor();
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${tab} offline=${offline} overflow at ${width}`);
        assert.equal(await page.locator('details[open]').count(),0,'Payloads should start collapsed');
        await expect(page.getByRole('link',{name:tab === 'chat' ? 'Chat' : 'Trace',exact:true})).toHaveAttribute('aria-current','page');
        assert.equal(await page.locator('#run-composer').count(),tab==='chat' ? 1 : 0);
        for (const control of await page.locator('.run-detail-view .cn-button, .run-detail-view input[type=search], .run-detail-view > .run-history summary').all()) {
          if (!await control.isVisible()) continue;
          const box = await control.boundingBox();
          assert.ok(box.height >= 44 && box.width >= 44, `Target too small: ${box.width}x${box.height}`);
        }
        if (tab === 'chat') {
          await expect(page.locator('.run-role-user')).toContainText('Implement a readable');
          await expect(page.locator('.run-role-assistant').first()).toContainText('Investigation');
          assert.equal(await page.getByText('Raw message',{exact:true}).count(),0);
        } else {
          await page.getByLabel('Filter activity').fill('collapsed-needle');
          await expect(page.locator('[data-trace-entry]:visible')).toHaveCount(1);
          await page.getByLabel('Filter activity').fill('First equal-sequence');
          await expect(page.locator('[data-trace-entry]:visible')).toHaveCount(1);
          await page.getByLabel('Filter activity').fill('no-match-fixture');
          await expect(page.locator('[data-trace-entry]:visible')).toHaveCount(0);
          await expect(page.locator('#trace-no-matches')).toBeVisible();
          await page.getByLabel('Filter activity').fill('');
          await expect(page.locator('#trace-no-matches')).toBeHidden();
        }
        // Send a real Datastar element patch from a controlled stream. The patch
        // uses the real renderer; only a diagnostic notice changes.
        const disclosure = page.locator(tab==='chat' ? '.run-chat-activity > summary' : '[data-trace-entry] > details > summary').first();
        await disclosure.focus();
        await page.keyboard.press('Enter');
        const id = await disclosure.evaluate(el=>el.parentElement.id);
        const input = page.locator(tab==='chat' ? '#run-draft' : '#trace-filter');
        await input.fill(tab==='chat' ? 'preserved draft' : 'Implement');
        await input.focus();
        await expect.poll(()=>streams.size).toBeGreaterThan(0);
        const source = await readFile(`${fixture}.${offline ? 'offline-' : ''}${tab}`, 'utf8');
        const content = await page.evaluate(html => {
          const doc = new DOMParser().parseFromString(html, 'text/html');
          return doc.getElementById('run-detail-content').outerHTML + doc.getElementById('run-detail-controls').outerHTML;
        }, source);
        const patch = content.replace('Activity truncated to bounded retained history.', 'Fixture live update received.');
        for (const stream of streams) stream.write(`event: datastar-patch-elements\ndata: elements ${patch.replaceAll("\n", "\ndata: elements ")}\n\n`);
        await page.getByText('Fixture live update received.',{exact:true}).waitFor();
        assert.ok(await page.locator(`[id="${id}"]`).evaluate(el=>el.open),'Disclosure reset during morph');
        assert.ok(await input.evaluate(el=>document.activeElement===el),'Focus moved during morph');
        await expect(input).toHaveValue(tab==='chat' ? 'preserved draft' : 'Implement');
        if (process.env.JAWA_RUN_DETAIL_SCREENSHOTS && [390,1440].includes(width)) {
          // Restore default collapsed view for review screenshots.
          await page.goto(`${url}?tab=${tab}${offline ? '&offline=1' : ''}`);
          await mkdir(process.env.JAWA_RUN_DETAIL_SCREENSHOTS,{recursive:true});
          await page.screenshot({path:`${process.env.JAWA_RUN_DETAIL_SCREENSHOTS}/${tab}${offline ? '-offline' : ''}-${width}.png`,fullPage:true});
        }
      }
    }
    console.log(`Run detail ${width}: both tabs/offline, filtering, targets, overflow, keyboard disclosures and morph preservation passed`);
  }
  await context.close();
  const native = await browser.newContext({javaScriptEnabled:false});
  const page2 = await native.newPage();
  await page2.goto(`${url}?tab=chat`);
  await page2.getByRole('link',{name:'Trace',exact:true}).click();
  await expect(page2).toHaveURL(/tab=trace/);
  await page2.locator('[data-trace-entry] > details > summary').first().press('Enter');
  assert.equal(await page2.locator('details[open]').count(),1);
  await native.close();
} finally {
  for (const stream of streams) stream.end();
  if (browser) await browser.close();
  await new Promise(resolve=>server.close(resolve));
}
