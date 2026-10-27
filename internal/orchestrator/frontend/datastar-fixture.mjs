import {launchBrowser} from './browser.mjs';
import {chooseSelect} from './select.mjs';
import {execFileSync} from 'node:child_process';
import {createServer} from 'node:http';
import {readFile, mkdir} from 'node:fs/promises';
import {randomBytes} from 'node:crypto';
import assert from 'node:assert/strict';
import {expect} from 'playwright/test';

// All declarations come from the production Go renderer, including dialogs.
// Only provider I/O and finite SSE responses are simulated on this loopback host.
export async function runScenario(scenario) {
  const dir = new URL('./node_modules/.datastar-fixture/', import.meta.url);
  execFileSync('go', ['test', '../../../internal/orchestrator', '-run', '^TestDatastarBrowserFixture$', '-count=1'], {env: {...process.env, JAWA_DATASTAR_FIXTURE: dir.pathname}});
  const fixture = async name => readFile(new URL(name, dir), 'utf8');
  const frame = (html, mode = 'outer', selector = '') => `event: datastar-patch-elements\ndata: mode ${mode}\n${selector ? `data: selector ${selector}\n` : ''}data: elements ${html.replaceAll('\n', '\ndata: elements ')}\n\n`;
  const signals = values => `event: datastar-patch-signals\ndata: signals ${JSON.stringify(values)}\n\n`;
  const remove = op => `event: datastar-patch-elements\ndata: mode remove\ndata: selector #dialog-${op}\n\n`;
  const requests = [], opens = [], snapshots = [];
  let reject = true, hold = false, pending = [], issueDelay = 0;
  const streams = new Set();
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://localhost');
    try {
      if (url.pathname.startsWith('/assets/')) {
        res.setHeader('Content-Type', url.pathname.endsWith('.js') ? 'text/javascript' : url.pathname.endsWith('.css') ? 'text/css' : 'application/octet-stream');
        res.end(await readFile(new URL(`..${url.pathname}`, import.meta.url))); return;
      }
      if (url.pathname === '/sw.js') {res.writeHead(404).end(); return;}
      if (url.pathname === '/events') {res.writeHead(200, {'Content-Type': 'text/event-stream'}); res.write(': connected\n\n'); streams.add(res); res.on('close', () => streams.delete(res)); return;}
      if (url.pathname === '/snapshot') {snapshots.push(url); res.writeHead(200, {'Content-Type': 'text/event-stream'}).end(': current\n\n'); return;}
      if (url.pathname === '/dialogs/card/issues') {
        if (issueDelay) await new Promise(resolve => setTimeout(resolve, issueDelay));
        res.writeHead(200, {'Content-Type': 'text/event-stream'}).end(frame(await fixture('issues.html'), 'replace') + frame(`<input id="card-issue-submission" type="hidden" name="issue_submission" value="${randomBytes(32).toString('hex')}">`) + signals({_issues: [{url: 'https://github.com/example/fixture/issues/1', title: 'Issue title', body: 'Issue body'}]})); return;
      }
      if (url.pathname === '/dialogs/assign/agents') {res.writeHead(200, {'Content-Type': 'text/event-stream'}).end(frame(await fixture('agents.html'), 'replace')); return;}
      if (url.pathname.startsWith('/dialogs/')) {
        const name = url.pathname.slice('/dialogs/'.length), op = randomBytes(16).toString('hex'); opens.push({name, op, url});
        const html = (await fixture(`${name}.html`)).replaceAll('a'.repeat(32), op);
        res.writeHead(200, {'Content-Type': 'text/event-stream'}).end(frame(html, 'inner', '#dialog-slot')); return;
      }
      if (req.method === 'POST') {
        let body = ''; for await (const chunk of req) body += chunk;
        const form = Object.fromEntries(new URLSearchParams(body)); requests.push({path: url.pathname, form});
        if (hold) await new Promise(resolve => pending.push(resolve));
        const name = url.pathname === '/projects' ? 'project' : url.pathname === '/cards' ? 'card' : ({'/cards/force-ready': 'ready', '/cards/force-stop': 'force'})[url.pathname] || url.pathname.split('/').pop();
        let response = reject ? frame(`<div role="alert" class="notice dialog-error" id="${name}-error-${form.dialog_operation}">Fixture rejection</div>`) : ': accepted\n\n';
        if (!reject && form.dialog_operation) response += remove(form.dialog_operation);
        if (url.pathname === '/prs/refresh') response += signals({prRefreshing: false});
        if (!reject && url.pathname === '/settings/nats') response += signals({natsPassword: ''});
        if (!reject && url.pathname === '/cards/planning/reply') response += signals({planningReply: ''});
        if (!reject && url.pathname === '/cards/guide' && !form.dialog_operation) response += signals({guidanceReply: ''});
        res.writeHead(200, {'Content-Type': 'text/event-stream'}).end(response); return;
      }
      res.setHeader('Content-Type', 'text/html'); res.end(await fixture(url.pathname === '/settings' ? 'settings.html' : url.pathname === '/planning' ? 'planning.html' : 'board.html'));
    } catch {res.writeHead(500).end('Fixture unavailable');}
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const browser = await launchBrowser();
  const page = await browser.newPage({viewport: {width: 1440, height: 1000}, serviceWorkers: 'block'});
  page.setDefaultTimeout(6000);
  const errors = []; page.on('pageerror', e => errors.push(e.message));
  const base = `http://127.0.0.1:${server.address().port}`;
  const shot = async name => {
    if (!process.env.JAWA_JS_REMOVAL_SCREENSHOTS) return;
    await mkdir(process.env.JAWA_JS_REMOVAL_SCREENSHOTS, {recursive: true});
    await page.screenshot({path: `${process.env.JAWA_JS_REMOVAL_SCREENSHOTS}/${name}.png`, fullPage: true});
  };
  const patch = html => {for (const stream of streams) stream.write(frame(html));};
  const open = async name => {
    // A Datastar GET opener is the same transport used by every real trigger.
    await page.evaluate(name => {const button = document.createElement('button'); button.id = 'fixture-open'; button.textContent = 'Open fixture'; button.setAttribute('data-on:click', `@get('/dialogs/${name}')`); document.body.append(button);}, name);
    await page.locator('#fixture-open').click(); await page.locator('#fixture-open').evaluate(el => el.remove());
    await expect(page.locator('[role=dialog]')).toBeVisible(); return opens.at(-1).op;
  };
  const close = async () => {await page.keyboard.press('Escape'); await expect(page.locator('[role=dialog]')).toHaveCount(0); await expect(page.locator('[data-slot=dialog-portal]')).toHaveCount(0);};
  try {
    await page.goto(base);
    if (['dialogs', 'merge', 'fix', 'ui'].includes(scenario)) {
      for (const name of scenario === 'merge' ? ['merge'] : scenario === 'fix' ? ['fix'] : ['project', 'card', 'delete', 'cancel', 'force', 'ready', 'guide', 'fix', 'merge', 'assign', 'apikeys']) {
        const first = await open(name);
        assert.match(first, /^[a-f0-9]{32}$/);
        assert.equal(await page.locator(`[id="${name}-error-${first}"]`).count(), 1);
        for (const checkbox of await page.locator('[role=dialog] [role=checkbox]').all()) if (name !== 'card') assert.equal(await checkbox.getAttribute('aria-checked'), 'false');
        if (name === 'merge') {
          assert.equal(await page.locator('[name=method]').inputValue(), 'merge');
          await page.locator('#merge-method').click(); await expect(page.getByRole('listbox')).toBeVisible();
          assert.equal(await page.getByRole('listbox').evaluate(el => !!el.closest('[data-slot=dialog-portal]')), true);
          await shot('dialog-merge-1440'); await page.keyboard.press('Escape'); await expect(page.getByRole('dialog')).toBeVisible();
          await chooseSelect(page, '#merge-method', 'squash');
        }
        await shot(`dialog-${name}-1440`); await close();
        const second = await open(name); assert.notEqual(second, first);
        if (name === 'merge') assert.equal(await page.locator('[name=method]').inputValue(), 'merge');
        await close();
      }
      // Real form fields, inline failure, duplicate lock, cancellation and stale success.
      const first = await open('fix'); await page.locator('#fix-message').fill('Keep my draft');
      hold = true; const submit = page.getByRole('button', {name: 'Send to Fix', exact: true});
      await submit.dblclick({force: true}); await expect.poll(() => requests.length).toBe(1); assert.equal(await submit.isDisabled(), true);
      pending.shift()(); await expect(page.locator('[role=dialog] .dialog-error')).toBeVisible(); assert.equal(await page.locator('#fix-message').inputValue(), 'Keep my draft'); await shot('dialog-error-1440');
      await expect(submit).toBeEnabled(); await submit.click(); await expect.poll(() => requests.length).toBe(2);
      await close(); const next = await open('fix'); assert.notEqual(next, first);
      reject = false; pending.shift()(); await page.waitForTimeout(100); await expect(page.getByRole('dialog')).toBeVisible(); await shot('dialog-reopen-1440');
      hold = false; await page.locator('#fix-message').fill('Confirmed'); await page.getByRole('button', {name: 'Send to Fix', exact: true}).click(); await expect(page.getByRole('dialog')).toHaveCount(0);
    }
    if (['issues', 'recovery'].includes(scenario)) {
      await open('card'); await expect(page.locator('#card-issue-status')).toHaveText('Choose an issue, or leave No linked issue selected.');
      const issueSelect = page.locator('#card-issue');
      await issueSelect.click();
      await expect(page.getByRole('listbox')).toBeVisible();
      await shot('card-dialog-issues-1440');
      await page.keyboard.press('Escape');
      await expect(issueSelect).toHaveAttribute('aria-expanded', 'false');
      await expect(page.getByRole('listbox')).toBeHidden();
      // Closing restores trigger focus asynchronously. Reopening before that
      // restoration can send ArrowDown to the closing popup instead.
      await expect(issueSelect).toBeFocused();
      await expect(page.getByRole('dialog')).toBeVisible();
      await chooseSelect(page, '#card-issue', 'https://github.com/example/fixture/issues/1');
      await expect(page.getByRole('checkbox', {name: 'Create issue', exact: true})).toBeHidden(); await page.getByRole('button', {name: 'Autofill', exact: true}).click();
      await expect(page.locator('#card-title')).toHaveValue('Issue title'); await expect(page.locator('#card-description')).toHaveValue('Issue body'); await shot('card-dialog-linked-1440');
      await page.getByRole('button', {name: 'Create card', exact: true}).click(); await expect.poll(() => requests.length).toBe(1);
      assert.equal(requests[0].form.project_id, 'project'); assert.equal(requests[0].form.issue_url, 'https://github.com/example/fixture/issues/1'); assert.equal(requests[0].form.create_issue, undefined);
      patch(await fixture('blocked-issues.html')); patch(await fixture('recovery.html')); await expect(page.locator('#card-create-issue')).toBeDisabled(); await shot('card-dialog-recovery-1440');
      await chooseSelect(page, '#issue-recovery-action', 'unlinked');
      const form = await page.locator('#card-dialog form').evaluate(el => Object.fromEntries(new FormData(el))); assert.equal(form.create_issue, undefined); assert.equal(form.issue_recovery, 'unlinked');
      await close();
    }
    if (['assignment', 'agents'].includes(scenario)) {
      await open('assign'); const submit = page.locator('[data-agent-submit]'); await expect(submit).toBeDisabled();
      await page.locator('#assignment-agent').click(); await expect(page.getByRole('option', {name: /disabled-agent/})).toHaveAttribute('aria-disabled', 'true'); await shot('assign-dialog-1440'); await page.keyboard.press('Escape');
      await chooseSelect(page, '#assignment-agent', 'busy-agent', true); await expect(submit).toBeDisabled();
      await page.locator('#assign-review-planning').locator('xpath=preceding-sibling::span[1]').click(); await expect(submit).toBeEnabled();
      await submit.click(); await expect.poll(() => requests.length).toBe(1); assert.equal(requests[0].form.expected_revision, '2'); assert.equal(requests[0].form.acknowledge, 'yes'); assert.equal(requests[0].form.target_agent_id, 'busy-agent');
      await expect(page.getByRole('alert')).toBeVisible(); await expect(page.locator('#assignment-agent')).toHaveAttribute('aria-expanded', 'false');
      await page.getByRole('button', {name: 'Refresh agents'}).click(); await expect(page.locator('#assignment-agent-field')).toHaveCount(1); await expect(submit).toBeDisabled();
      assert.equal(await page.locator('#assign-review-planning').isChecked(), true); await close();
    }
    if (['menu', 'cards'].includes(scenario)) {
      const trigger = page.locator('[data-card-id=todo] button[aria-haspopup=menu]');
      await trigger.focus(); await page.keyboard.press('ArrowDown'); await expect(page.getByRole('menu')).toBeVisible();
      await expect(page.locator('#board-content')).toHaveAttribute('data-ignore-morph');
      assert.equal(await page.getByRole('menuitem').first().evaluate(el => document.activeElement === el), true);
      await page.keyboard.press('ArrowDown'); await shot('menu-keyboard-1440'); await page.keyboard.press('End'); await page.keyboard.press('Home'); await page.keyboard.press('d');
      await expect(page.locator('[role=menuitem]:focus')).toContainText('Delete');
      const before = snapshots.length; await page.keyboard.press('Escape'); await expect(page.getByRole('menu')).toHaveCount(0); await expect.poll(() => snapshots.length).toBe(before + 1); await expect(trigger).toBeFocused(); await expect(page.locator('#board-content')).not.toHaveAttribute('data-ignore-morph');
      await trigger.click(); const beforeAction = snapshots.length; await page.getByRole('menuitem', {name: 'Plan first', exact: true}).click(); await expect.poll(() => requests.length).toBe(1);
      assert.equal(requests[0].path, '/cards/plan'); assert.equal(requests[0].form.card_id, 'todo'); assert.equal(requests[0].form._csrf, 'fixture-csrf'); assert.equal(snapshots.length, beforeAction);
      await expect(page.getByRole('menu')).toHaveCount(0); await trigger.click(); await expect(page.getByRole('menu')).toBeVisible(); await page.getByRole('menuitem', {name: 'Build with agent', exact: true}).click(); await expect(page.getByRole('dialog')).toBeVisible(); await close(); await expect(trigger).toBeFocused();
      // The gesture's event bridge must flush bound hidden fields before FormData.
      await page.locator('.board').evaluate(el => el.dispatchEvent(new CustomEvent('board-move', {bubbles: true, detail: {card: 'todo', status: 'Planning', position: 2}})));
      await expect.poll(() => requests.length).toBe(2); assert.equal(requests[1].form.card_id, 'todo'); assert.equal(requests[1].form.status, 'Planning'); assert.equal(requests[1].form.position, '2');
      await page.locator('.board').evaluate(el => el.dispatchEvent(new CustomEvent('board-drag', {bubbles: true, detail: {active: true}}))); await expect(page.locator('#board-content')).toHaveAttribute('data-ignore-morph');
      await page.locator('.board').evaluate(el => el.dispatchEvent(new CustomEvent('board-drag', {bubbles: true, detail: {active: false}}))); await expect(page.locator('#board-content')).not.toHaveAttribute('data-ignore-morph');
      await shot('board-1440');
    }
    if (scenario === 'refresh') {
      hold = true; const button = page.locator('.pr-refresh-form button'); await button.dblclick({force: true}); await expect.poll(() => requests.length).toBe(1); await expect(button).toBeDisabled();
      const html = await page.locator('#board-content').evaluate(el => el.outerHTML); patch(html); await expect(button).toBeDisabled(); await shot('pr-refresh-pending-1440');
      pending.shift()(); await expect(button).toBeEnabled(); assert.equal(requests.length, 1);
    }
    if (['components', 'forms'].includes(scenario)) {
      await page.goto(`${base}/settings`);
      await page.evaluate(() => {HTMLFormElement.prototype.reset = () => {throw new Error('form.reset is forbidden');};});
      const password = page.locator('#nats-password'); await password.fill('test-long-password');
      const save = page.getByRole('button', {name: 'Save credentials and continue'}); await save.click(); await expect.poll(() => requests.length).toBe(1); await expect(password).toHaveValue('test-long-password');
      reject = false; await save.click(); await expect(password).toHaveValue('');
      const input = page.locator('#runtime-auto-resume'), root = input.locator('xpath=preceding-sibling::span[1]');
      await root.click(); assert.equal(await input.isChecked(), false); assert.equal(await root.getAttribute('aria-checked'), 'false');
      // A changed server default must replace the switch declaration, not only
      // alter its hidden input (the old document-wide observer is gone).
      const html = await page.locator('#settings-content').evaluate(el => {const clone = el.cloneNode(true); const input = clone.querySelector('#runtime-auto-resume'); input.removeAttribute('checked'); const field = input.closest('.switch-field'); field.id = 'runtime-auto-resume-off'; return clone.outerHTML;});
      patch(html); await expect(input).not.toBeChecked(); await expect(root).toHaveAttribute('aria-checked', 'false');
      const concurrency = page.getByLabel('Global per-agent concurrency override');
      await expect(concurrency).toHaveValue('');
      await expect(concurrency).toHaveAttribute('min', '1');
      await expect(concurrency).toHaveAttribute('max', '1024');
      await concurrency.fill('0');
      assert.equal(await concurrency.evaluate(el => el.checkValidity()), false);
      await concurrency.fill('1025');
      assert.equal(await concurrency.evaluate(el => el.checkValidity()), false);
      await concurrency.fill('12');
      const concurrencyForm = concurrency.locator('xpath=ancestor::form');
      assert.equal(await concurrencyForm.evaluate(el => new FormData(el).get('agent-concurrency')), '12');
      await concurrency.fill('');
      assert.equal(await concurrency.evaluate(el => el.checkValidity()), true);
      assert.equal(await concurrencyForm.evaluate(el => new FormData(el).get('agent-concurrency')), '');
      await page.goto(`${base}/planning`); await chooseSelect(page, '[id^=plan-revision-][role=combobox]', '2');
      const id = await page.locator('[id^=plan-revision-][role=combobox]').getAttribute('id'); const html2 = await page.locator('#planning-content').evaluate(el => el.outerHTML); patch(html2); await expect(page.locator(`[id="${id}"]`)).toHaveCount(1);
    }
    if (scenario === 'auto-merge') {
      await page.goto(`${base}/planning`);
      for (const mode of ['inherit', 'on', 'off']) {await chooseSelect(page, '#card-merge', mode, true); const form = await page.locator('[data-auto-merge-form]').evaluate(el => Object.fromEntries(new FormData(el))); assert.equal(form.auto_merge_mode, mode);}
      await page.locator('#card-merge').click(); await shot('auto-merge-planning-1440'); await page.keyboard.press('Escape');
    }
    assert.deepEqual(errors, [], 'No runtime errors');
    console.log(`Datastar ${scenario}: production component markup and SSE passed`);
  } finally {
    pending.splice(0).forEach(resolve => resolve()); streams.forEach(stream => stream.end()); await browser.close(); server.closeAllConnections(); await new Promise(resolve => server.close(resolve));
  }
}
