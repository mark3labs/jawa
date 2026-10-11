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
<jawa-issue-selector>
<label>Issue <select id="card-issue" name="issue_url"><option value="">No linked issue</option></select></label>
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
  const issue = page.locator('#card-issue');
  const status = page.locator('#card-issue-status');
  const project = page.locator('#card-project');
  const refresh = page.locator('[data-issue-refresh]');
  const issueURL = 'https://github.com/example/repo/issues/7';
  const entries = [{number: 7, title: '<img src=x onerror="window.injected=true"> & "title"', url: issueURL}];
  const waitRequests = count => page.waitForFunction(n => requests.length === n, count);
  const reply = (index, issues = entries, code = 200) => page.evaluate(
    ({index, issues, code}) => window.reply(index, issues, code), {index, issues, code}
  );
  const waitLoaded = () => page.waitForFunction(() => !document.querySelector('#card-issue').disabled);
  const waitStatus = text => page.waitForFunction(
    text => document.querySelector('#card-issue-status').textContent.includes(text), text
  );
  const submitWithoutLink = async () => {
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
  await reply(0);
  await waitLoaded();
  assert.equal(await issue.locator('option').nth(1).textContent(), `#7 ${entries[0].title}`);
  assert.equal(await page.locator('jawa-issue-selector img').count(), 0);
  assert.equal(await page.evaluate(() => window.injected), undefined);
  await issue.selectOption(issueURL);
  await page.click('#submit');
  assert.equal(await page.evaluate(() => submission.issue_url), issueURL);
  await issue.selectOption('');
  await submitWithoutLink();
  await issue.selectOption(issueURL);

  // Changing projects clears the link synchronously and aborts older requests.
  await project.selectOption('beta & other');
  await waitRequests(2);
  assert.equal(await issue.inputValue(), '');
  assert.equal(await issue.isDisabled(), true);
  assert.equal(await page.evaluate(() => requests[1].url), '/projects/issues?project_id=beta%20%26%20other');
  await project.selectOption('alpha');
  await waitRequests(3);
  assert.equal(await page.evaluate(() => requests[1].signal.aborted), true);
  await reply(2);
  await waitLoaded();
  await issue.selectOption(issueURL);
  await reply(1, [{number: 9, title: 'Stale project', url: 'https://example.com/stale'}]);
  await page.waitForTimeout(50);
  assert.equal(await issue.inputValue(), issueURL);
  assert.equal(await issue.locator('option').count(), 2);

  // Refresh clears selection, safe failures remain optional, and retry works.
  await refresh.click();
  await waitRequests(4);
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
  await reply(5);
  await waitLoaded();

  // Empty and malformed responses are safe; a blank project makes no request.
  await refresh.click();
  await waitRequests(7);
  await reply(6, []);
  await waitStatus('No open issues');
  assert.equal(await issue.isDisabled(), true);
  await submitWithoutLink();
  await refresh.click();
  await waitRequests(8);
  await reply(7, null);
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
  await reply(11);
  await waitLoaded();
  await reply(10, []);
  await page.waitForTimeout(50);
  assert.equal(await issue.isEnabled(), true);

  // The 25-second deadline invalidates late responses, and completed timers clear.
  await page.clock.install();
  await refresh.click();
  await waitRequests(13);
  await page.clock.fastForward(25000);
  await waitStatus('timed out');
  assert.equal(await page.evaluate(() => requests[12].signal.aborted), true);
  await submitWithoutLink();
  await reply(12);
  await page.waitForTimeout(50);
  assert.equal(await issue.isDisabled(), true);
  await refresh.click();
  await waitRequests(14);
  await reply(13);
  await waitLoaded();
  await page.clock.fastForward(25000);
  assert.equal(await issue.isEnabled(), true);
  assert.equal(await page.evaluate(() => requests[13].signal.aborted), false);

  // Disposal aborts and removes observers/listeners; even ignored aborts cannot update UI.
  await refresh.click();
  await waitRequests(15);
  await page.evaluate(() => { window.stop(); window.stop(); });
  assert.equal(await page.evaluate(() => requests[14].signal.aborted), true);
  const snapshot = await status.textContent();
  await reply(14);
  await page.waitForTimeout(50);
  assert.equal(await issue.locator('option').count(), 1);
  assert.equal(await issue.isDisabled(), true);
  await refresh.click();
  await project.selectOption('beta & other');
  await page.click('#reset');
  await page.click('#close');
  await page.click('#open');
  await page.clock.fastForward(30000);
  assert.equal(await page.evaluate(() => requests.length), 15);
  assert.equal(await status.textContent(), snapshot);
  assert.deepEqual(errors, []);
  console.log('Issue selector tests passed (static fixture, no orchestrator).');
} finally {
  await browser?.close();
  await new Promise(resolve => server.close(resolve));
}
