// Integration with shadcn-templ beta.14; leave selection and popup cleanup to
// the pinned runtime. Lookup content globally because it can be portaled.
export const selectTrigger = input => input?.closest('[data-select-field]')?.querySelector('[data-slot="select-trigger"]');
export const selectInput = trigger => trigger?.closest('[data-select-field]')?.querySelector('input[name]');
export const selectContent = trigger => document.getElementById(trigger?.dataset.templControls);

export function closeSelect(trigger) {
  if (trigger?.getAttribute('aria-expanded') !== 'true') return;
  // useDismiss owns Escape, including focus restoration and releasing locks.
  selectContent(trigger)?.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}));
}
export function setSelectDisabled(input, disabled) {
  const trigger = selectTrigger(input);
  if (!trigger) return;
  if (disabled) closeSelect(trigger);
  input.disabled = disabled;
  trigger.disabled = disabled;
  trigger.toggleAttribute('data-disabled', disabled);
  trigger.setAttribute('aria-disabled', String(disabled));
  trigger.tabIndex = disabled ? -1 : 0;
}
export function setSelectValue(input, value) {
  const trigger = selectTrigger(input);
  if (trigger) window.templ.select.setValue(trigger, value);
}

// Preserve portal ownership at the declaration site. Explicit dialog targets
// keep popup/backdrop inside the native modal top layer on every reopen.
const renderPortal = window.templ.portal.render;
window.templ.portal.render = (node, target) => {
  const popup = node.querySelector('[data-slot="select-content"]');
  const trigger = popup && [...document.querySelectorAll('[data-slot="select-trigger"]')]
    .find(el => el.dataset.templControls === popup.id);
  node._jawaSelectTrigger = trigger;
  return renderPortal(node, target || trigger?.closest('dialog'));
};

document.addEventListener('close', event => {
  if (!event.target.matches?.('dialog')) return;
  event.target.querySelectorAll('[data-slot="select-trigger"]').forEach(closeSelect);
}, true);
// Escape dismisses the popup before the native dialog's cancel default action.
document.addEventListener('cancel', event => {
  if (event.target.querySelector?.('[data-slot="select-trigger"][aria-expanded="true"]')) event.preventDefault();
}, true);

// setValue emits change only when the input differs. The browser has already
// restored it here, so reset never starts duplicate issue requests.
document.addEventListener('reset', event => {
  if (!(event.target instanceof HTMLFormElement)) return;
  queueMicrotask(() => {
    for (const trigger of event.target.querySelectorAll('[data-slot="select-trigger"]')) {
      closeSelect(trigger);
      const input = selectInput(trigger);
      setSelectValue(input, input.value);
      if (input.name === 'provider') input.dispatchEvent(new Event('change', {bubbles: true}));
    }
  });
});

// A keyed SSE morph can retain the declaration wrapper/trigger but insert new
// Content. Upstream ownership alone then keeps the old portal alive. Retire
// that portal and remount the retained trigger so beta.14 binds the new list.
new MutationObserver(() => {
  for (const node of document.querySelectorAll('[data-base-ui-portal]')) {
    const trigger = node._jawaSelectTrigger;
    if (!trigger) continue;
    const popup = node.querySelector('[data-slot="select-content"]');
    const owner = node._templPortalOwner;
    const replacement = owner?.querySelector('[data-slot="select-content"]');
    if (trigger.isConnected && (!replacement || replacement === popup)) continue;
    const positioner = popup?.parentElement;
    if (positioner) window.templ.internalBackdrop.remove(positioner);
    window.templ.portal.remove(node);
    if (trigger.isConnected) trigger.replaceWith(trigger.cloneNode(true));
  }
}).observe(document.documentElement, {subtree: true, childList: true});
