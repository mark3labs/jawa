import { expect } from 'playwright/test';
import { launchBrowser } from './browser.mjs';
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
  if (req.method === 'POST' && url.pathname.endsWith('/chat')) {
    sends++;
    let body = '';
    for await (const chunk of req) body += chunk;
    const text = new URLSearchParams(body).get('text');
    const messageID = new URLSearchParams(body).get('message_id');
    messageIDs.push(messageID);
    await new Promise(resolve => { releaseSend = resolve; });
    res.writeHead(200, {'Content-Type':'text/event-stream'});
    if (failSend) { res.end(': uncertain response\n\n'); return; }
    res.end(`event: datastar-patch-signals\ndata: signals ${JSON.stringify({runConfirmedDraft:text,runAcceptedMessage:messageID})}\n\n`);
    return;
  }
  const asset = /^\/assets\/([a-zA-Z0-9_.-]+)$/.exec(url.pathname);
  const variant = `${url.pathname.startsWith('/runs/planning/') ? 'planning-' : url.searchParams.get('offline') ? 'offline-' : ''}${url.searchParams.get('tab') === 'trace' ? 'trace' : 'chat'}`;
  const file = asset ? fileURLToPath(new URL(asset[1], assets)) : url.pathname.startsWith('/runs/') ? `${fixture}.${variant}` : null;
  if (!file) { res.writeHead(404).end(); return; }
  try {
    res.writeHead(200, {'Content-Type':asset ? url.pathname.endsWith('.css') ? 'text/css' : url.pathname.endsWith('.js') ? 'text/javascript' : 'application/octet-stream' : 'text/html'}).end(await readFile(file));
  } catch { res.writeHead(404).end(); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser;
let sends = 0;
let releaseSend;
let failSend = false;
const messageIDs = [];
const url = `http://127.0.0.1:${server.address().port}/runs/11111111-1111-4111-8111-111111111111`;
try {
  browser = await launchBrowser();
  const context = await browser.newContext({serviceWorkers:'block'});
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  const externalRequests = [];
  page.on('request', req => { if (!req.url().startsWith(url.split('/runs/')[0])) externalRequests.push(req.url()); });
  page.on('console', m => { if(m.type() === 'error') console.error(m.text()); });
  page.on('pageerror', e => console.error(e.message));
  for (const width of [320,390,768,1024,1440]) {
    await page.setViewportSize({width,height:1000});
    for (const tab of ['chat','trace']) {
      for (const offline of [false,true]) {
        await page.goto(`${url}?tab=${tab}${offline ? '&offline=1' : ''}`);
        await page.locator('#run-detail-content').waitFor();
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), `${tab} offline=${offline} overflow at ${width}`);
        assert.equal(await page.locator('details[open]:visible').count(),0,'Payloads should start collapsed');
        await expect(page.getByRole('link',{name:tab === 'chat' ? 'Chat' : 'Trace',exact:true})).toHaveAttribute('aria-current','page');
        assert.equal(await page.locator('#run-composer').count(),tab==='chat' ? 1 : 0);
        for (const control of await page.locator('.run-detail-view .cn-button, .run-detail-view .cn-toggle, .run-detail-view input[type=search], .run-detail-view > .run-history summary').all()) {
          // Inert controls, like the hidden scroll-to-end button, take no input.
          if (!await control.isVisible() || await control.evaluate(el => !!el.closest('[inert]'))) continue;
          const box = await control.boundingBox();
          assert.ok(box.height >= 44 && box.width >= 44, `Target too small: ${box.width}x${box.height}`);
        }
        if (tab === 'chat') {
          await expect(page.locator('.run-role-user').first()).toContainText('Implement a readable');
          await expect(page.locator('#chat-delivery-pending-message')).toContainText('Queued offline message');
          await expect(page.locator('#chat-delivery-pending-message .cn-badge')).toHaveText('Queued');
          await expect(page.locator('#chat-delivery-failed-message .cn-badge')).toHaveText('Failed');
          await expect(page.locator('#chat-delivery-pending-message')).not.toContainText('followup');
          await expect(page.locator('.run-role-assistant').first()).toContainText('Investigation');
          assert.equal(await page.getByText('Message content',{exact:true}).count(), 0, 'Raw payloads belong in Trace');
          // Calls and results share one row; failures are marked on the row.
          await expect(page.locator('.run-tool')).toHaveCount(2);
          await expect(page.locator('.run-tool-error')).toHaveCount(2);
          await expect(page.locator('.run-tool').first().locator(':scope > summary')).toContainText('go test ./...');
          await expect(page.locator('.run-tool').first().locator(':scope > summary [data-slot=trace-description]')).toHaveText('exit 1');
          await expect(page.locator('.run-tool').first().locator(':scope > summary [data-slot=trace-line]')).toHaveAttribute('data-status','error');
          await expect(page.locator('.run-reasoning > summary')).toContainText('Thought');
          assert.equal(await page.getByText('Unsupported content',{exact:false}).count(), 1, 'Only the fixture part with no known type is unsupported');
          await expect(page.locator('.planning-markdown [data-templ-copy-button]').first()).toBeVisible();
        } else {
          await page.getByLabel('Filter activity').fill('collapsed-needle');

          await expect(page.locator('[data-trace-entry]:visible')).toHaveCount(1);
          await page.getByLabel('Filter activity').fill('First equal-sequence');
          await expect(page.locator('[data-trace-entry]:visible')).toHaveCount(1);
          // Turn separators stay with their matching entries.
          await expect(page.locator('[data-trace-turn-header]:visible')).toHaveCount(1);
          await page.getByRole('button',{name:'Live',exact:true}).click();
          await expect(page.getByRole('button',{name:'Live',exact:true})).toHaveAttribute('aria-pressed','false');
          await expect(page.locator('[data-trace-entry]:visible')).toHaveCount(0);
          await expect(page.locator('[data-trace-turn-header]:visible')).toHaveCount(0);
          await page.getByRole('button',{name:'Live',exact:true}).click();
          await page.getByLabel('Filter activity').fill('no-match-fixture');
          await expect(page.locator('[data-trace-entry]:visible')).toHaveCount(0);
          await expect(page.locator('.run-trace-source:visible')).toHaveCount(0);
          assert.equal(await page.locator('#trace-no-matches').count(),0);
          await page.getByLabel('Filter activity').fill('');
          await page.getByRole('button',{name:'Snapshot',exact:true}).click();
          await expect(page.locator('#run-trace-snapshot')).toBeHidden();
          await expect(page.locator('#run-trace-events')).toBeVisible();
          await page.getByRole('button',{name:'Snapshot',exact:true}).click();
          await expect(page.locator('#run-trace-snapshot')).toBeVisible();

        }
        // Send a real Datastar element patch from a controlled stream. The patch
        // uses the real renderer; only a diagnostic notice changes.
        const disclosure = page.locator(tab==='chat' ? '.run-tool > summary' : '[data-trace-entry] details > summary').first();
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
        const patch = content.replaceAll('Investigation', 'Fixture live update received');
        for (const stream of streams) stream.write(`event: datastar-patch-elements\ndata: elements ${patch.replaceAll("\n", "\ndata: elements ")}\n\n`);
        await expect(page.locator('#run-detail-content')).toContainText('Fixture live update received');
        assert.ok(await page.locator(`[id="${id}"]`).evaluate(el=>el.open),'Disclosure reset during morph');
        if (tab === 'chat') {
          // The stream appends new committed entries without re-sending the transcript.
          for (const stream of streams) stream.write(`event: datastar-patch-elements\ndata: mode append\ndata: selector #run-messages\ndata: elements <div id="appended-entry-${width}-${offline}" class="run-chat-entry">Appended committed entry</div>\n\n`);
          await expect(page.locator(`#run-messages > #appended-entry-${width}-${offline}`)).toHaveText('Appended committed entry');
          assert.ok(await page.locator(`[id="${id}"]`).evaluate(el=>el.open),'Disclosure reset during append');
        }
        assert.ok(await input.evaluate(el=>document.activeElement===el),'Focus moved during morph');
        await expect(input).toHaveValue(tab==='chat' ? 'preserved draft' : 'Implement');
        if (process.env.JAWA_RUN_DETAIL_SCREENSHOTS && [390,1440].includes(width)) {
          // Restore default collapsed view for review screenshots.
          await page.goto(`${url}?tab=${tab}${offline ? '&offline=1' : ''}`);
          await mkdir(process.env.JAWA_RUN_DETAIL_SCREENSHOTS,{recursive:true});
          if (tab === 'chat') await page.locator('#run-transcript').evaluate(el => { el.scrollTop = 0; el.dispatchEvent(new Event('scroll')); });
          await page.screenshot({path:`${process.env.JAWA_RUN_DETAIL_SCREENSHOTS}/${tab}${offline ? '-offline' : ''}-${width}.png`,fullPage:true});
        }
      }
    }
    console.log(`Run detail ${width}: both tabs/offline, filtering, targets, overflow, keyboard disclosures and morph preservation passed`);
  }
  await page.goto(`${url}?tab=chat`);
  const draft = page.locator('#run-draft');
  const send = page.locator('button[form="run-composer"]');
  await draft.press('Control+Enter');
  assert.equal(sends,0,'Native required validation bypassed');
  await draft.fill('line');
  await draft.press('End');
  await draft.press('Enter');
  await expect(draft).toHaveValue('line\n');
  assert.equal(sends,0,'Plain Enter submitted');
  await draft.evaluate(el => el.dispatchEvent(new KeyboardEvent('keydown',{key:'Enter',ctrlKey:true,isComposing:true,bubbles:true})));
  assert.equal(sends,0,'IME submitted');
  const small = await draft.evaluate(el => el.clientHeight);
  await draft.fill('growing\n'.repeat(20));
  assert.ok(await draft.evaluate(el => el.clientHeight) > small);
  await draft.fill('submitted draft');
  await draft.press('Control+Enter');
  await expect.poll(()=>sends).toBe(1);
  await expect(send).toBeDisabled();
  await draft.fill('new draft while pending');
  await draft.press('Control+Enter');
  assert.equal(sends,1,'Duplicate pending send');
  releaseSend();
  await expect(send).toBeEnabled();
  await expect(draft).toHaveValue('new draft while pending');
  await draft.press('Control+Enter');
  await expect.poll(()=>sends).toBe(2);
  releaseSend();
  await expect(draft).toHaveValue('');
  failSend = true;
  await draft.fill('uncertain draft');
  await draft.press('Control+Enter');
  await expect.poll(()=>sends).toBe(3);
  releaseSend();
  await expect(send).toBeEnabled();
  await expect(draft).toHaveValue('uncertain draft');
  const uncertainID = await page.locator('#run-message-id').inputValue();
  await draft.press('Control+Enter');
  await expect.poll(()=>sends).toBe(4);
  assert.equal(messageIDs[3], uncertainID, 'Uncertain retry changed message identity');
  releaseSend();
  await expect(send).toBeEnabled();
  assert.notEqual(messageIDs[0],messageIDs[1],'New draft reused accepted identity');
  await page.emulateMedia({reducedMotion:'reduce'});
  const transcript = page.locator('#run-transcript');
  await expect.poll(()=>transcript.evaluate(el => el.scrollHeight-el.clientHeight-el.scrollTop)).toBeLessThan(32);
  // A reader can only scroll away once the scroller's own follow has settled;
  // a programmatic scroll during it is absorbed by the follow.
  await expect.poll(()=>transcript.evaluate(el => el.hasAttribute('data-autoscrolling'))).toBe(false);
  await transcript.evaluate(el => {el.scrollTop=0; el.dispatchEvent(new Event('scroll'));});
  await page.waitForTimeout(100);
  await page.locator('.run-role-assistant').first().evaluate(el => el.append(document.createTextNode('appended text')));
  await page.waitForTimeout(100);
  assert.equal(await transcript.evaluate(el => el.scrollTop),0,'Scrolled-up reader moved');
  await transcript.evaluate(el => {el.scrollTop=el.scrollHeight; el.dispatchEvent(new Event('scroll'));});
  await page.waitForTimeout(100);
  await page.locator('#run-transcript').evaluate(el => {
    const article = document.createElement('article');
    article.textContent = 'new message '.repeat(300);
    el.querySelector('[data-slot=message-scroller-content]').append(article);
  });
  await expect.poll(()=>transcript.evaluate(el => el.scrollHeight-el.clientHeight-el.scrollTop)).toBeLessThan(32);
  // Expanded JSON/code stays within the transcript and short viewports remain
  // navigable in normal document flow rather than clipping the composer.
  await page.setViewportSize({width:390,height:500});
  await page.locator('.run-tool').first().evaluate(el => el.open = true);
  // Long output scrolls inside its ScrollArea, not the page.
  await expect(page.locator('.run-tool').first().locator('[data-slot=scroll-area]').first()).toBeVisible();
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth+1));
  await send.scrollIntoViewIfNeeded();
  await expect(send).toBeVisible();
  assert.deepEqual(externalRequests, [], 'Remote media/assets requested');
  await page.goto(`${url}?tab=chat&offline=1`);
  await page.locator('#run-draft').fill('offline draft');
  await page.locator('#run-draft').press('Control+Enter');
  await expect.poll(()=>sends).toBe(5);
 releaseSend();
 await expect(page.locator('button[form="run-composer"]')).toBeEnabled();
  const planningURL=url.replace('/runs/','/runs/planning/');
  for (const width of [320,390,768,1024,1440]) {
    await page.setViewportSize({width,height:1000});
    for (const tab of ['chat','trace']) {
      await page.goto(`${planningURL}?tab=${tab}`);
      await page.locator('#run-detail-content').waitFor();
      assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth <= innerWidth+1), `planning ${tab} overflow ${width}`);
      await expect(page.locator('#run-composer')).toHaveCount(tab==='chat' ? 1:0);
      await expect(page.getByRole('button',{name:/Stop current turn/})).toHaveCount(0);
      await expect(page.getByText('Awaiting review',{exact:true})).toBeVisible();
      const link=page.getByRole('link',{name:tab==='chat' ? 'Trace':'Chat',exact:true});
      await link.focus(); await page.keyboard.press('Enter');
      await expect(page).toHaveURL(new RegExp(`/runs/planning/.*tab=${tab==='chat' ? 'trace':'chat'}`));
      if (process.env.JAWA_RUN_DETAIL_SCREENSHOTS && [390,1440].includes(width)) {
        await mkdir(process.env.JAWA_RUN_DETAIL_SCREENSHOTS,{recursive:true});
        await page.goto(`${planningURL}?tab=${tab}`);
        await page.screenshot({path:`${process.env.JAWA_RUN_DETAIL_SCREENSHOTS}/planning-${tab}-${width}.png`,fullPage:true});
      }
    }
  }
  await context.close();
  const native = await browser.newContext({javaScriptEnabled:false});
  const page2 = await native.newPage();
  await page2.goto(`${url}?tab=chat`);
  await page2.getByRole('link',{name:'Trace',exact:true}).click();
  await expect(page2).toHaveURL(/tab=trace/);
  await page2.locator('[data-trace-entry] details > summary').first().press('Enter');
  assert.equal(await page2.locator('details[open]:visible').count(),1);
  await page2.goto(`${planningURL}?tab=chat`);
  await page2.getByRole('link',{name:'Trace',exact:true}).click();
  await expect(page2).toHaveURL(/\/runs\/planning\/.*tab=trace/);
  await expect(page2.locator('#run-composer')).toHaveCount(0);
  await native.close();
} finally {
  for (const stream of streams) stream.end();
  if (browser) await browser.close();
  await new Promise(resolve=>server.close(resolve));
}
