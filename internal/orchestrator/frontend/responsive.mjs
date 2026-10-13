import { chromium } from 'playwright';
import assert from 'node:assert/strict';

// Run against an authenticated live server; setup is opt-in for a disposable DB:
// JAWA_SMOKE_URL=http://127.0.0.1:18080 JAWA_RESPONSIVE_SETUP=1 node responsive.mjs
// Existing servers can supply JAWA_RESPONSIVE_STORAGE_STATE (Playwright JSON).
const base = process.env.JAWA_SMOKE_URL;
assert.ok(base, 'Set JAWA_SMOKE_URL to the live server URL');
const setup = process.env.JAWA_RESPONSIVE_SETUP === '1';
const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM_PATH || '/etc/profiles/per-user/space_cowboy/bin/chromium',
  headless: true, args: ['--no-sandbox'],
});
const context = await browser.newContext({
  ...(process.env.JAWA_RESPONSIVE_STORAGE_STATE ? { storageState: process.env.JAWA_RESPONSIVE_STORAGE_STATE } : {}),
  viewport: { width: 390, height: 844 }, hasTouch: true,
});
const page = await context.newPage();
page.setDefaultTimeout(10000);
const errors = [];
page.on('pageerror', error => errors.push(error.message));
const overflow = async label => {
  const outside = await page.evaluate(() => [...document.querySelectorAll('body *')]
    .filter(el => { const r = el.getBoundingClientRect(); return r.width && (r.right > innerWidth + 1 || r.left < -1); })
    .map(el => `${el.tagName}.${el.className}`).slice(0, 12));
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${label}: document overflow (${outside.join(', ')})`);
};
const target = async locator => {
  const box = await locator.boundingBox();
  assert.ok(box && box.width >= 44 && box.height >= 44, `${locator}: target below 44px`);
};
let boardPath = process.env.JAWA_RESPONSIVE_BOARD_PATH || '/board';
try {
  if (setup) {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`${base}/setup`);
    await page.getByLabel('Username', { exact: true }).fill('responsive-admin');
    await page.getByLabel('Password', { exact: true }).fill('responsive-test-password-123');
    await page.getByRole('button', { name: 'Create administrator' }).click();
    await page.locator('#settings-content').waitFor();
    await page.getByLabel('New password').fill('responsive-agent-password-123');
    await page.getByRole('button', { name: 'Save credentials and continue' }).click();
    await page.locator('#board-content').waitFor();
    await page.getByRole('button', { name: 'New project', exact: true }).first().click();
    await page.getByLabel('Name', { exact: true }).fill(`Responsive ${'long-project-name-'.repeat(5)}`);
    await page.getByLabel('Repository name').fill('example/responsive');
    await page.getByRole('button', { name: 'Create project', exact: true }).click();
    await page.locator('.lanes').waitFor();
    boardPath = new URL(page.url()).pathname + new URL(page.url()).search;
    await page.locator('.topbar').getByRole('button', { name: 'New card', exact: true }).click();
    await page.getByLabel('Title', { exact: true }).fill(`Responsive ${'long-title-'.repeat(15)}`);
    await page.getByLabel('Description', {exact: true}).fill('Long description for responsive layout');
    await page.locator('#card-create-issue').uncheck();
    await page.getByRole('button', { name: 'Create card', exact: true }).click();
    await page.locator('.task-card').waitFor();
    await page.locator('.task-card button[aria-haspopup="menu"]').click();
    await page.getByRole('menuitem', { name: 'Start work', exact: true }).click();
    await page.locator('.task-card a[title="View runs"]').waitFor();
    if (process.env.JAWA_RESPONSIVE_SAVE_STATE) await context.storageState({ path: process.env.JAWA_RESPONSIVE_SAVE_STATE });
  }

  for (const [width, height] of [[320, 640], [390, 844], [768, 1024], [1024, 768], [1440, 1000]]) {
    await page.setViewportSize({ width, height });
    for (const view of ['board', 'runs', 'agents', 'settings']) {
      await page.goto(base + (view === 'board' ? boardPath : `/${view}`));
      await page.locator(`#${view}-content`).waitFor();
      await page.evaluate(() => document.fonts.ready);
      await overflow(`${view}/${width}`);
      await target(page.getByRole('button', { name: 'New project', exact: true }).first());
      for (const control of await page.locator('.nav-item, .topbar .btn, .agent-chip, .tab').all()) {
        await target(control);
      }
      if (view === 'board') {
        if (await page.locator('.lanes').count()) {
          const columns = await page.locator('.lanes').evaluate(el => getComputedStyle(el).gridTemplateColumns.split(' ').length);
          assert.equal(columns, width <= 600 ? 1 : width < 1400 ? 2 : 5);
        }
        for (const id of ['project-dialog', 'card-dialog', 'delete-dialog', 'cancel-dialog']) {
          await page.evaluate(id => document.getElementById(id).showModal(), id);
          // Reduced motion avoids timing-dependent dialog bounds checks.
          await page.emulateMedia({ reducedMotion: 'reduce' });
          const dialog = page.locator(`#${id}`);
          await dialog.waitFor({ state: 'visible' });
          await page.waitForTimeout(160);
          const bounds = await dialog.boundingBox();
          assert.ok(bounds.x >= 0 && bounds.y >= 0 && bounds.x + bounds.width <= width && bounds.y + bounds.height <= height, `${id}/${width}: outside viewport`);
          assert.ok(await dialog.evaluate(el => el.scrollWidth <= el.clientWidth), `${id}: horizontal overflow`);
          for (const input of await dialog.locator('input:not([type="hidden"]):not([type="checkbox"]), select, textarea').all()) {
            if (await input.isVisible()) assert.ok(await input.evaluate(el => parseFloat(getComputedStyle(el).fontSize) >= 16), 'input below 16px');
          }
          await page.evaluate(id => document.getElementById(id).close(), id);
        }
        for (const button of await page.locator('.task-card .icon-btn').all()) {
          assert.equal(await button.evaluate(el => getComputedStyle(el).opacity), '1');
          await target(button);
        }
        if (await page.locator('.task-card').count()) {
          await page.locator('.task-card button[aria-haspopup="menu"]').first().click();
          const menu = page.locator('.menu:popover-open');
          await menu.waitFor();
          const sizes = await menu.evaluate(el => [...el.querySelectorAll('.menu-item')]
            .map(item => item.getBoundingClientRect()).filter(rect => rect.width)
            .map(rect => ({width: rect.width, height: rect.height})));
          assert.ok(sizes.length > 0);
          assert.ok(sizes.every(rect => rect.width >= 44 && rect.height >= 44), 'menu touch targets');
          await page.keyboard.press('Escape');
        }
      }
      if (view === 'runs') {
        const runs = await page.locator('.run').all();
        assert.ok(runs.length, 'Use a server with at least one run (or opt into disposable setup)');
        for (const run of runs) {
          for (const field of ['main', 'state', 'attempt', 'agent', 'pr', 'time']) {
            assert.ok(await run.locator(`.run-${field}`).isVisible(), `run ${field} hidden at ${width}`);
          }
          await run.locator('summary').click();
          await run.locator('.facts').waitFor({ state: 'visible' });
        }
        await overflow(`expanded runs/${width}`);
      }
    }
  }
  // Archive/restore a real card through Datastar, then verify archive filtering.
  await page.goto(base + boardPath);
  const archivedCard = page.locator('.task-card').first();
  const cardID = await archivedCard.getAttribute('id');
  await archivedCard.getByRole('button', { name: 'Archive', exact: true }).click();
  await page.locator(`#${cardID}`).waitFor({ state: 'detached' });
  await page.goto(base + '/archive');
  await page.locator(`#${cardID}`).waitFor();
  await page.getByRole('textbox', { name: 'Filter cards', exact: true }).fill('no-match-for-archive-test');
  await page.locator(`#${cardID}`).waitFor({ state: 'hidden' });
  await page.getByRole('textbox', { name: 'Filter cards', exact: true }).fill('');
  await page.locator(`#${cardID}`).getByRole('button', { name: 'Restore', exact: true }).click();
  await page.locator(`#${cardID}`).waitFor({ state: 'detached' });
  await page.goto(base + boardPath);
  await page.locator(`#${cardID}`).waitFor();

  // Fine-pointer mobile must still have generous targets and visible actions.
  const fine = await browser.newContext({ viewport: { width: 390, height: 844 }, storageState: await context.storageState() });
  const finePage = await fine.newPage();
  await finePage.goto(base + boardPath);
  await finePage.locator('#board-content').waitFor();
  await target(finePage.getByRole('button', { name: 'New project', exact: true }).first());
  await fine.close();
  assert.deepEqual(errors, [], 'browser errors');
  console.log('PASS: embedded production assets; 320/390/768/1024/1440 layouts, navigation, touch targets, run fields, menus, dialogs, 16px inputs and overflow');
} finally {
  await browser.close();
}
