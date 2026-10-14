// Register before importing Datastar: its initial DOM scan executes effects
// synchronously. Keep this module free of dependencies on the Datastar runtime.
// Datastar supplies boolean state; the native input owns values and validity.
window.jawaSyncAcknowledgement = (host, checked, required) => {
  const root = host.querySelector('[data-slot="checkbox"]');
  const input = root?.nextElementSibling;
  if (!input) return;
  input.required = !!required;
  root.setAttribute('aria-required', String(!!required));
  window.templ.checkbox.setChecked(root, !!checked);
};
