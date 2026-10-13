import assert from 'node:assert/strict';
import { test } from 'node:test';
import { chromium } from 'playwright';
import { browserOptions, launchBrowser, launchPersistentContext } from './browser.mjs';

test('shared Chromium options and both launch modes', async () => {
  const previous = process.env.CHROMIUM_PATH;
  const launch = chromium.launch;
  const persistent = chromium.launchPersistentContext;
  try {
    delete process.env.CHROMIUM_PATH;
    assert.deepEqual(browserOptions(), { headless: true, args: ['--no-sandbox'] });
    process.env.CHROMIUM_PATH = '';
    assert.equal(Object.hasOwn(browserOptions(), 'executablePath'), false);
    process.env.CHROMIUM_PATH = '/custom/chromium';
    assert.equal(browserOptions().executablePath, '/custom/chromium');
    const overrides = { headless: false, args: [], executablePath: '/override', timeout: 20000 };
    assert.deepEqual(browserOptions(overrides), overrides);
    chromium.launch = async options => options;
    chromium.launchPersistentContext = async (profile, options) => ({ profile, options });
    assert.deepEqual(await launchBrowser(), browserOptions());
    assert.deepEqual(await launchPersistentContext('profile', { timeout: 20000 }), {
      profile: 'profile', options: browserOptions({ timeout: 20000 }),
    });
  } finally {
    chromium.launch = launch;
    chromium.launchPersistentContext = persistent;
    if (previous === undefined) delete process.env.CHROMIUM_PATH;
    else process.env.CHROMIUM_PATH = previous;
  }
});
