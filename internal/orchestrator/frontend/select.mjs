// Exercise the actual combobox runtime, never write only the form input.
export async function chooseSelect(page, selector, value, keyboard = false) {
  const trigger = page.locator(selector);
  await trigger.focus();
  await page.keyboard.press('ArrowDown');
  const id = await trigger.getAttribute('data-templ-controls');
  const popup = page.locator(`[id="${id}"]`);
  await popup.waitFor({state: 'visible'});
  const options = popup.getByRole('option');
  const index = await options.evaluateAll((els, value) => els.findIndex(el => el.dataset.templValue === value), value);
  if (index < 0) throw new Error(`Missing Select option ${value}`);
  if (keyboard) {
    await options.first().focus();
    await page.keyboard.press('Home');
    for (let i = 0; i < index; i++) await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
  } else await options.nth(index).click();
  await popup.waitFor({state: 'hidden'});
  await page.waitForFunction(selector => document.querySelector(selector)?.getAttribute('aria-expanded') === 'false', selector);
}
