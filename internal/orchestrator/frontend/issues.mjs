import { chromium } from 'playwright';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';

// Serve only the fixture and the actual ES module: no orchestrator required.
const fixture = `<!doctype html>
<button id="open" onclick="document.querySelector('dialog').showModal()">Open</button>
<dialog><form>
<label>Project <select id="card-project" name="project_id">
<option value="alpha" selected>Alpha</option><option value="beta & other">Beta</option>
<option value="">Choose a project</option></select></label>
<input id="card-title" name="title" value="Manual title">
<textarea id="card-description" name="description">Manual description</textarea>
<input type="hidden" name="issue_submission" value="initial">
<jawa-issue-selector>
<label data-create-issue><input id="card-create-issue" name="create_issue" type="checkbox" value="yes" checked>Create issue</label>
<p data-create-help>Create and link on submit.</p><div id="card-issue-recovery"></div>
<label>Issue <select id="card-issue" name="issue_url"><option value="">No linked issue</option></select></label>
<button data-issue-autofill type="button" disabled>Autofill</button>
<p id="card-issue-status" role="status"></p>
<button data-issue-refresh type="button">Refresh issues</button>
</jawa-issue-selector>
<button id="submit" type="submit">Create card</button>
<button id="reset" type="reset">Reset</button>
<button id="close" type="button" onclick="this.closest('dialog').close()">Close</button>
</form></dialog>
<script type="module">
import { setupIssueSelector } from '/issue-selector.js';
// Deferred responses deliberately ignore abort, exercising generation guards.
window.requests = [];
window.fetch = (url, {signal}) => new Promise((resolve, reject) => {
  window.requests.push({url, signal, resolve, reject});
});
window.reply = (index, issues, status = 200) => requests[index].resolve(
  new Response(JSON.stringify({issues}), {status, headers: {'Content-Type': 'application/json'}})
);
window.stop = () => {};
setupIssueSelector(document.querySelector('jawa-issue-selector'), fn => window.stop = fn);
document.querySelector('form').addEventListener('submit', event => {
  event.preventDefault();
  window.submission = Object.fromEntries(new FormData(event.target));
});
window.ready = true;
</script>`;
const moduleSource = await readFile(new URL('./src/issue-selector.js', import.meta.url));
const server = createServer((req, res) => {
  if (req.url === '/issue-selector.js') {
    res.writeHead(200, {'Content-Type': 'text/javascript'}).end(moduleSource);
  } else if (req.url === '/') {
    res.writeHead(200, {'Content-Type': 'text/html'}).end(fixture);
  } else res.writeHead(404).end();
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
let browser;
try {
  browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {}),
    headless: true,
    args: ['--no-sandbox'],
  });
  const page = await browser.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(`http://127.0.0.1:${server.address().port}/`);
  await page.waitForFunction(() => window.ready);
  const create = page.locator('#card-create-issue');
  assert.equal(await create.isChecked(), true);
  const issue = page.locator('#card-issue');
  const status = page.locator('#card-issue-status');
  const project = page.locator('#card-project');
  const autofill = page.locator('[data-issue-autofill]');
  const title = page.locator('#card-title');
  const description = page.locator('#card-description');
  const draft = async () => [await title.inputValue(), await description.inputValue()];
  const refresh = page.locator('[data-issue-refresh]');
  const issueURL = 'https://github.com/example/repo/issues/7';
  const entries = [{number: 7, title: '<img src=x onerror="window.injected=true"> & "title"', url: issueURL, body: '  # 日本語\n**Markdown**\n<img src=x onerror="window.injected=true">  '}];
  const waitRequests = count => page.waitForFunction(n => requests.length === n, count);
  const reply = (index, issues = entries, code = 200) => page.evaluate(
    ({index, issues, code}) => window.reply(index, issues, code), {index, issues, code}
  );
  const waitLoaded = () => page.waitForFunction(() => !document.querySelector('#card-issue').disabled);
  const waitStatus = text => page.waitForFunction(
    text => document.querySelector('#card-issue-status').textContent.includes(text), text
  );
  const submitWithoutLink = async () => {
    const before = await draft();
    // Even synthetic clicks must re-check membership, not trust disabled UI.
    await autofill.dispatchEvent('click');
    assert.deepEqual(await draft(), before);
    assert.equal(await autofill.isDisabled(), true);
    assert.equal(await page.locator('#submit').isEnabled(), true);
    await page.click('#submit');
    const submitted = await page.evaluate(() => window.submission);
    assert.ok(!submitted.issue_url, 'loading/error/empty submissions must have no issue link');
  };

  // Opening loads the current project; safe option text submits the provider URL.
  await page.click('#open');
  await waitRequests(1);
  assert.equal(await page.evaluate(() => requests[0].url), '/projects/issues?project_id=alpha');
  assert.equal(await issue.isDisabled(), true);
  assert.match(await status.textContent(), /Loading/);
  await submitWithoutLink();
  assert.equal(await page.evaluate(() => submission.create_issue), 'yes');
  await create.uncheck();
  await reply(0);
  await waitLoaded();
  assert.equal(await issue.locator('option').nth(1).textContent(), `#7 ${entries[0].title}`);
  assert.equal(await page.locator('jawa-issue-selector img').count(), 0);
  assert.equal(await page.evaluate(() => window.injected), undefined);
  assert.equal(await autofill.isDisabled(), true);
  await issue.selectOption(issueURL);
  assert.equal(await create.isDisabled(), true);
  assert.equal(await create.isChecked(), false);
  assert.equal(await autofill.isEnabled(), true);
  assert.deepEqual(await draft(), ['Manual title', 'Manual description']);
  await autofill.focus();
  await page.keyboard.press('Enter');
  assert.deepEqual(await draft(), [entries[0].title, entries[0].body]);
  assert.equal(await page.evaluate(() => window.submission.title), 'Manual title', 'autofill must not submit');
  assert.equal(await page.locator('dialog img').count(), 0);
  await title.fill('Edited title');
  await description.fill('Edited description');
  await page.click('#submit');
  assert.deepEqual(await page.evaluate(() => [submission.title, submission.description]), ['Edited title', 'Edited description']);
  assert.equal(await page.evaluate(() => submission.issue_url), issueURL);
  await issue.selectOption('');
  assert.deepEqual(await draft(), ['Edited title', 'Edited description']);
  await submitWithoutLink();
  await issue.selectOption(issueURL);

  const edited = ['Edited title', 'Edited description'];

  // Changing projects clears the link synchronously and aborts older requests.
  await project.selectOption('beta & other');
  await waitRequests(2);
  assert.equal(await create.isChecked(), true);
  assert.equal(await autofill.isDisabled(), true);
  assert.deepEqual(await draft(), edited);
  assert.equal(await issue.inputValue(), '');
  assert.equal(await issue.isDisabled(), true);
  assert.equal(await page.evaluate(() => requests[1].url), '/projects/issues?project_id=beta%20%26%20other');
  await project.selectOption('alpha');
  await waitRequests(3);
  assert.equal(await page.evaluate(() => requests[1].signal.aborted), true);
  await reply(2);
  await waitLoaded();
  await issue.selectOption(issueURL);
  await reply(1, [{number: 9, title: 'Stale project', body: 'Stale body', url: 'https://example.com/stale'}]);
  await page.waitForTimeout(50);
  assert.equal(await issue.inputValue(), issueURL);
  assert.equal(await issue.locator('option').count(), 2);
  await autofill.click();
  assert.deepEqual(await draft(), [entries[0].title, entries[0].body], 'stale reply cannot replace copy source');
  await title.fill(edited[0]);
  await description.fill(edited[1]);

  // Refresh clears selection, safe failures remain optional, and retry works.
  await issue.selectOption('');
  await create.uncheck();
  await refresh.click();
  await waitRequests(4);
  assert.equal(await create.isChecked(), false);
  assert.equal(await autofill.isDisabled(), true);
  assert.deepEqual(await draft(), edited);
  assert.equal(await issue.inputValue(), '');
  await reply(3, [], 502);
  await waitStatus('Unable to load');
  assert.equal(await issue.isDisabled(), true);
  await submitWithoutLink();
  await refresh.click();
  await waitRequests(5);
  await page.evaluate(() => requests[4].reject(new Error('<script>private provider error</script>')));
  await waitStatus('Unable to load');
  assert.doesNotMatch(await status.textContent(), /private provider/);
  await refresh.click();
  await waitRequests(6);
  await reply(5, [{...entries[0], body: ''}]);
  await waitLoaded();
  await issue.selectOption(issueURL);
  await autofill.click();
  assert.deepEqual(await draft(), [entries[0].title, '']);
  await title.fill(edited[0]);
  await description.fill(edited[1]);

  // Empty and malformed responses are safe; a blank project makes no request.
  await refresh.click();
  await waitRequests(7);
  await reply(6, []);
  await waitStatus('No open issues');
  assert.equal(await issue.isDisabled(), true);
  await submitWithoutLink();
  await refresh.click();
  await waitRequests(8);
  await reply(7, [{...entries[0], body: null}]);
  await waitStatus('Unable to load');
  await project.selectOption('');
  await waitStatus('Choose a project');
  assert.equal(await page.evaluate(() => requests.length), 8);

  // Reset reloads the default project only after native reset has taken effect.
  await project.selectOption('beta & other');
  await waitRequests(9);
  await reply(8);
  await waitLoaded();
  await issue.selectOption(issueURL);
  await page.click('#reset');
  await waitRequests(10);
  assert.equal(await create.isChecked(), true);
  assert.equal(await autofill.isDisabled(), true);
  assert.deepEqual(await draft(), ['Manual title', 'Manual description']);
  assert.equal(await project.inputValue(), 'alpha');
  assert.equal(await issue.inputValue(), '');
  assert.equal(await page.evaluate(() => requests[9].url), '/projects/issues?project_id=alpha');
  await reply(9);
  await waitLoaded();
  await issue.selectOption(issueURL);

  // Closing cancels in-flight work; every reopening refreshes the list.
  await refresh.click();
  await waitRequests(11);
  await page.click('#close');
  await page.waitForFunction(() => requests[10].signal.aborted);
  await page.click('#open');
  await waitRequests(12);
  assert.equal(await autofill.isDisabled(), true);
  assert.deepEqual(await draft(), ['Manual title', 'Manual description']);
  await reply(11);
  await waitLoaded();
  await reply(10, []);
  await page.waitForTimeout(50);
  assert.equal(await issue.isEnabled(), true);

  // Pending submission preserves fields while preventing project/issue/intent changes.
  await page.evaluate(() => document.dispatchEvent(new CustomEvent('datastar-fetch', {detail:{type:'started',el:document.querySelector('form')}})));
  assert.equal(await create.isDisabled(), true);
  assert.equal(await project.isDisabled(), true);
  assert.equal(await issue.isDisabled(), true);
  assert.equal(await page.evaluate(() => new FormData(document.querySelector('form')).get('project_id')), 'alpha');
  await page.evaluate(() => document.dispatchEvent(new CustomEvent('datastar-fetch', {detail:{type:'finished',el:document.querySelector('form')}})));
  assert.equal(await project.isEnabled(), true);
  // Uncertain failure keeps draft and token; only explicit recovery is submitted.
  const tokenBefore = await page.locator('[name="issue_submission"]').inputValue();
  const draftBefore = await draft();
  await page.evaluate(() => {
    const recovery = document.querySelector('#card-issue-recovery');
    recovery.dataset.state = 'uncertain';
    recovery.innerHTML = '<select name="issue_recovery"><option value="">Choose recovery</option><option value="unlinked">Create unlinked</option></select><input name="recovery_url">';
  });
  await page.waitForFunction(() => document.querySelector('#card-create-issue').disabled);
  assert.deepEqual(await draft(), draftBefore);
  assert.equal(await page.locator('[name="issue_submission"]').inputValue(), tokenBefore);
  await page.locator('[name="issue_recovery"]').selectOption('unlinked');
  await page.click('#submit');
  assert.equal(await page.evaluate(() => submission.issue_recovery), 'unlinked');
  assert.equal(await page.evaluate(() => submission.create_issue), undefined);
  // Simulate confirmed success reset; no POST occurred on toggle, close or reset.
  await page.evaluate(() => HTMLFormElement.prototype.reset.call(document.querySelector('form')));
  await waitRequests(13);
  await reply(12);
  await waitLoaded();
  assert.equal(await create.isChecked(), true);
  assert.equal(await page.locator('[name="issue_recovery"]').count(), 0);

  // The 25-second deadline invalidates late responses, and completed timers clear.
  await page.clock.install();
  await refresh.click();
  await waitRequests(14);
  await page.clock.fastForward(25000);
  await waitStatus('timed out');
  assert.equal(await page.evaluate(() => requests[13].signal.aborted), true);
  await submitWithoutLink();
  await reply(13);
  await page.waitForTimeout(50);
  assert.equal(await issue.isDisabled(), true);
  assert.equal(await autofill.isDisabled(), true);
  await refresh.click();
  await waitRequests(15);
  await reply(14);
  await waitLoaded();
  await page.clock.fastForward(25000);
  assert.equal(await issue.isEnabled(), true);
  assert.equal(await page.evaluate(() => requests[14].signal.aborted), false);

  // Disposal aborts and removes observers/listeners; even ignored aborts cannot update UI.
  await refresh.click();
  await waitRequests(16);
  await page.evaluate(() => { window.stop(); window.stop(); });
  assert.equal(await page.evaluate(() => requests[15].signal.aborted), true);
  assert.equal(await autofill.isDisabled(), true);
  const disposedDraft = await draft();
  await autofill.dispatchEvent('click');
  assert.deepEqual(await draft(), disposedDraft);
  const snapshot = await status.textContent();
  await reply(15);
  await page.waitForTimeout(50);
  assert.equal(await issue.locator('option').count(), 1);
  assert.equal(await issue.isDisabled(), true);
  assert.equal(await autofill.isDisabled(), true);
  await refresh.click();
  await project.selectOption('beta & other');
  await page.click('#reset');
  await page.click('#close');
  await page.click('#open');
  await page.clock.fastForward(30000);
  assert.equal(await page.evaluate(() => requests.length), 16);
  assert.equal(await status.textContent(), snapshot);
  assert.deepEqual(errors, []);
  console.log('Issue selector tests passed (static fixture, no orchestrator).');
} finally {
  await browser?.close();
  await new Promise(resolve => server.close(resolve));
}
