import { chromium } from 'playwright';

// Leave executablePath unset to use Playwright's bundled browser by default.
export const browserOptions = (opts = {}) => ({
  headless: true,
  args: ['--no-sandbox'],
  ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
  ...opts,
});

export const launchBrowser = (opts = {}) => chromium.launch(browserOptions(opts));

// Authentication checks need a real persistent profile across browser relaunches.
export const launchPersistentContext = (profile, opts = {}) =>
  chromium.launchPersistentContext(profile, browserOptions(opts));
