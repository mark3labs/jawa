import {launchBrowser} from './browser.mjs';
import {execFileSync} from 'node:child_process';
import {createServer} from 'node:http';
import {readFile, mkdir} from 'node:fs/promises';
import assert from 'node:assert/strict';
import {expect} from 'playwright/test';

const dir = new URL('./node_modules/.mermaid/', import.meta.url);
execFileSync('go', ['test', '../../../internal/orchestrator', '-run', '^TestMermaidBrowserFixture$', '-count=1'], {env: {...process.env, JAWA_MERMAID_FIXTURE: dir.pathname}});
const fixture = name => readFile(new URL(name, dir), 'utf8');
const frame = html => `event: datastar-patch-elements\ndata: elements ${html.replaceAll('\n', '\ndata: elements ')}\n\n`;
let next = 'snapshot.html';
const server = createServer(async (req, res) => {
  const path = new URL(req.url, 'http://localhost').pathname;
  if (path.startsWith('/assets/')) {
    res.setHeader('Content-Type', path.endsWith('.js') ? 'text/javascript' : path.endsWith('.css') ? 'text/css' : 'application/octet-stream');
    res.end(await readFile(new URL(`..${path}`, import.meta.url))); return;
  }
  if (path === '/dialog') {
    const html = await fixture('dialog.html');
    res.writeHead(200, {'Content-Type': 'text/event-stream'}).end(`event: datastar-patch-elements\ndata: mode inner\ndata: selector #dialog-slot\ndata: elements ${html.replaceAll('\n', '\ndata: elements ')}\n\n`); return;
  }
  if (path === '/patch') {res.writeHead(200, {'Content-Type': 'text/event-stream'}).end(frame(await fixture(next))); return;}
  if (path === '/events' || path === '/snapshot') {res.writeHead(200, {'Content-Type': 'text/event-stream'}).end(': current\n\n'); return;}
  if (path === '/sw.js') {res.writeHead(404).end(); return;}
  res.setHeader('Content-Type', 'text/html'); res.end(await fixture('page.html'));
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const browser = await launchBrowser();
const page = await browser.newPage({viewport: {width: 1440, height: 1000}, serviceWorkers: 'block'});
page.setDefaultTimeout(15000);
const base = `http://127.0.0.1:${server.address().port}`;
const errors = [], external = [];
page.on('pageerror', e => errors.push(e.message));
page.on('console', message => {if (message.type() === 'error') errors.push(message.text());});
await page.route('**/*', route => {
  if (!route.request().url().startsWith(base + '/')) {external.push(route.request().url()); return route.abort();}
  return route.continue();
});
const diagrams = page.locator('#planning-content .planning-diagram-viewport svg');
const patch = async name => {
  next = name;
  await page.evaluate(() => {
    const button = document.createElement('button'); button.id = 'patch-fixture'; button.textContent = 'Patch';
    button.setAttribute('data-on:click', "@get('/patch')"); document.body.append(button);
  });
  await page.locator('#patch-fixture').click();
  await page.locator('#patch-fixture').evaluate(el => el.remove());
};
const uniqueIDs = async () => {
  const ids = await page.locator('.planning-diagram-viewport [id]').evaluateAll(els => els.map(el => el.id));
  assert.equal(ids.length, new Set(ids).size, 'duplicate SVG IDs');
};
try {
  await page.goto(base);
  await expect(diagrams).toHaveCount(9);
  await uniqueIDs();
  assert.equal(await page.locator('pre > code:not(.language-mermaid)').first().textContent(), 'fmt.Println(1)\n');
  await expect(page.locator('#planning-content strong').first()).toHaveText('Bold');
  const before = await diagrams.first().innerHTML();
  await page.evaluate(() => {
    window.renderAttempts = 0;
    new MutationObserver(records => {
      for (const record of records) for (const node of record.addedNodes)
        if (node.classList?.contains('planning-diagram-staging')) window.renderAttempts++;
    }).observe(document.body, {childList: true});
  });
  await page.locator('[data-bind\\:guidance-reply]').fill('Preserve this draft');
  for (let i = 0; i < 3; i++) {await patch('snapshot.html'); await expect(diagrams).toHaveCount(9);}
  await expect(page.locator('[data-bind\\:guidance-reply]')).toHaveValue('Preserve this draft');
  assert.equal((await diagrams.first().innerHTML()).replaceAll(/jawa-diagram-\d+/g, 'diagram'), before.replaceAll(/jawa-diagram-\d+/g, 'diagram'), 'unchanged snapshot restores cached output');
  await uniqueIDs();
  assert.equal(await page.evaluate(() => window.renderAttempts), 0, 'unchanged fences must use cache');
  // Native disclosures work with keyboard; revision SVGs were rendered hidden.
  const source = page.locator('.planning-diagram summary').first();
  await source.focus(); await page.keyboard.press('Enter');
  await expect(source.locator('..')).toHaveAttribute('open', '');
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({width, height: 1000});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true, `overflow at ${width}`);
    if (process.env.JAWA_MERMAID_SCREENSHOTS) {
      await mkdir(process.env.JAWA_MERMAID_SCREENSHOTS, {recursive: true});
      await page.screenshot({path: `${process.env.JAWA_MERMAID_SCREENSHOTS}/mermaid-${width}.png`, fullPage: true});
    }
  }
  await patch('changed.html'); await expect(diagrams).toHaveCount(8);
  await expect(diagrams.first()).toContainText('Alice');
  await uniqueIDs();
  await patch('failures.html');
  await expect(page.locator('#planning-content .planning-diagram [role=status]')).toHaveCount(9);
  await expect(diagrams).toHaveCount(8);
  await patch('fix.html');
  await expect(page.locator('#planning-content .planning-diagram [role=status]')).toHaveCount(9);
  await expect(diagrams).toHaveCount(8);
  const failedAttempts = await page.evaluate(() => window.renderAttempts);
  await patch('failures.html');
  await expect(page.locator('#planning-content .planning-diagram [role=status]')).toHaveCount(9);
  assert.equal(await page.evaluate(() => window.renderAttempts), failedAttempts, 'failed diagrams must not be retried');
  // Outside the card/dialog ownership boundary, even marked code stays literal.
  await page.evaluate(() => {
    const runs = document.createElement('section'); runs.id = 'runs-fixture';
    runs.innerHTML = '<div class="planning-markdown"><pre><code class="language-mermaid">flowchart LR\\nA-->B</code></pre></div>';
    document.body.append(runs);
  });
  await page.waitForTimeout(100);
  await expect(page.locator('#runs-fixture .planning-diagram')).toHaveCount(0);
  // Dialog scope is outside the morph root; use production assignment markup.
  await page.evaluate(() => {
    const button = document.createElement('button'); button.id = 'dialog-fixture'; button.textContent = 'Review plan';
    button.setAttribute('data-on:click', "@get('/dialog')"); document.body.append(button);
  });
  await page.locator('#dialog-fixture').click();
  await expect(page.locator('[role=dialog] .planning-diagram-viewport svg')).toHaveCount(1);
  await uniqueIDs();
  // Removed/changed queued targets must not receive stale asynchronous output.
  await page.locator('#planning-content').evaluate(el => {
    const markdown = document.createElement('div'); markdown.className = 'planning-markdown';
    markdown.innerHTML = '<pre><code class="language-mermaid">flowchart LR\nX-->Y</code></pre>';
    el.append(markdown); queueMicrotask(() => markdown.remove());
  });
  await page.waitForTimeout(200);
  assert.equal(await page.evaluate(() => window.pwned), undefined);
  assert.deepEqual(errors, []);
  assert.deepEqual(external, []);
  const noJS = await browser.newContext({javaScriptEnabled: false});
  const fallback = await noJS.newPage(); await fallback.goto(base);
  await expect(fallback.locator('code.language-mermaid').first()).toBeVisible();
  await expect(fallback.locator('.planning-markdown svg')).toHaveCount(0);
  await noJS.close();
  console.log('Mermaid production markup, SSE, fallback, security and responsive checks passed');
} finally {await browser.close(); await new Promise(resolve => server.close(resolve));}
