// Draft switches never mutate saved policy until their containing form submits.
export function setupAutoMerge(host, cleanup) {
  const canonical = host.querySelector('[name="auto_merge_mode"]');
  const inheritRoot = host.querySelector('[data-merge-inherit]');
  const onRoot = host.querySelector('[data-merge-on]');
  const inherit = inheritRoot.nextElementSibling;
  const on = onRoot.nextElementSibling;
  const sync = () => {
    on.disabled = inherit.checked;
    onRoot.toggleAttribute('data-disabled', inherit.checked);
    onRoot.tabIndex = inherit.checked ? -1 : 0;
    onRoot.querySelector('[data-slot="switch-thumb"]').toggleAttribute('data-disabled', inherit.checked);
    onRoot.setAttribute('aria-disabled', String(inherit.checked));
    canonical.value = inherit.checked ? 'inherit' : on.checked ? 'on' : 'off';
  };
  host.addEventListener('change', sync);
  const reset = () => queueMicrotask(() => {
    inherit.checked = host.dataset.mode === 'inherit';
    on.checked = host.dataset.mode === 'on';
    inherit.dispatchEvent(new Event('change', {bubbles:true}));
    on.dispatchEvent(new Event('change', {bubbles:true}));
    sync();
  });
  host.closest('form').addEventListener('reset', reset);
  cleanup(() => {
    host.removeEventListener('change', sync);
    host.closest('form')?.removeEventListener('reset', reset);
  });
  sync();
}
