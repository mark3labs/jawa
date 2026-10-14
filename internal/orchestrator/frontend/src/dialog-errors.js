// Registered before Datastar evaluates shell effects. Each opening gets a new
// patch root: an old response cannot target a different operation's alert.
window.jawaDialog = (dialog, open) => {
  if (open && dialog.open) return;
  const error = dialog.querySelector('.dialog-error');
  const operation = dialog.querySelector('[name="dialog_operation"]');
  if (!open && !dialog.open && !operation.value) return;
  const succeeded = error.hasAttribute('data-succeeded');
  error.removeAttribute('data-succeeded');
  error.textContent = '';
  error.hidden = true;
  if (open) {
    dialog.jawaRequest = new AbortController();
    operation.value = Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('');
    error.id = `${dialog.id.replace('-dialog', '')}-error-${operation.value}`;
    dialog.showModal();
  } else {
    dialog.jawaRequest?.abort();
    if (succeeded) dialog.querySelector('form').reset();
    operation.value = '';
    error.id = `${dialog.id.replace('-dialog', '')}-error`;
    dialog.close();
  }
};
