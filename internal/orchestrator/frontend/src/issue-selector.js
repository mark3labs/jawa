// Standalone behavior; the caller supplies the custom-element cleanup registrar.
export function setupIssueSelector(host, cleanup) {
  const form = host.closest('form');
  const dialog = host.closest('dialog');
  const project = form?.querySelector('#card-project');
  const select = host.querySelector('#card-issue');
  const status = host.querySelector('#card-issue-status');
  const refresh = host.querySelector('[data-issue-refresh]');
  const autofill = host.querySelector('[data-issue-autofill]');
  const title = form?.querySelector('#card-title');
  const description = form?.querySelector('#card-description');
  if (!autofill || !title || !description) return;
  if (!form || !dialog || !project || !select || !status || !refresh) return;

  const create = form.querySelector('[name="create_issue"]');
  const token = form.querySelector('[name="issue_submission"]');
  const createRoot = host.querySelector('[data-create-issue] [data-slot="checkbox"]');
  const createLabel = host.querySelector('[data-create-issue]');
  const createHelp = host.querySelector('[data-create-help]');
  const recovery = () => host.querySelector('#card-issue-recovery');
  let pending = false;
  let pendingFields = [];
  const sources = new Map();
  const freshToken = () => {
    if (token) token.value = Array.from(crypto.getRandomValues(new Uint8Array(32)), n => n.toString(16).padStart(2, '0')).join('');
  };
  function updateIntent() {
    const linked = !!select.value;
    const recovering = !!recovery()?.querySelector('[name="issue_recovery"]')?.value;
    const blocked = ['reserved', 'uncertain', 'rejected'].includes(recovery()?.dataset.state);
    if (create) {
      create.disabled = pending || linked || recovering || blocked;
      if (linked || recovering || blocked) create.checked = false;
    }
    if (createRoot && create) {
      // Programmatic resets/locks must also update the shadcn visible control.
      window.templ?.checkbox?.setChecked(createRoot, create.checked);
      createRoot.setAttribute('aria-disabled', String(create.disabled));
      createRoot.toggleAttribute('data-disabled', create.disabled);
      createRoot.tabIndex = create.disabled ? -1 : 0;
    }
    if (createLabel) createLabel.hidden = linked;
    if (createHelp) createHelp.hidden = linked;
    autofill.hidden = !linked;
    const help = host.querySelector('#card-issue-autofill-help');
    if (help) help.hidden = !linked;
  }
  function resetIntent() {
    if (create) create.checked = true;
    if (recovery()) { recovery().replaceChildren(); delete recovery().dataset.state; }
    freshToken();
    updateIntent();
  }
  function projectChanged() { clear(); resetIntent(); load(); }
  function recoveryChanged() { updateIntent(); }
  function fetchState(event) {
    if (event.detail.el !== form) return;
    if (event.detail.type === 'started') pending = true;
    else if (event.detail.type === 'finished') pending = false;
    else return;
    if (pending) {
      // Datastar emits started before constructing FormData. Preserve successful
      // control values in hidden mirrors before locking the visible controls.
      for (const control of [project, select, create]) {
        if (!control?.name || control.disabled || (control.type === 'checkbox' && !control.checked)) continue;
        const mirror = document.createElement('input');
        mirror.type = 'hidden'; mirror.name = control.name; mirror.value = control.value;
        form.append(mirror); pendingFields.push(mirror);
      }
    } else {
      pendingFields.forEach(field => field.remove()); pendingFields = [];
    }
    project.disabled = pending;
    refresh.disabled = pending;
    if (pending) { cancel(); select.disabled = true; }
    else select.disabled = sources.size === 0;
    updateAutofill();
    updateIntent();
  }
  const recoveryObserver = new MutationObserver(updateIntent);
  if (recovery()) recoveryObserver.observe(host, {childList: true, subtree: true, attributes: true, attributeFilter: ['data-state']});
  host.addEventListener('change', recoveryChanged);
  document.addEventListener('datastar-fetch', fetchState);
  let loadedProject;
  let disposed = false;
  let generation = 0;
  let controller;
  let timeout;
  let resetTimer;

  function cancel() {
    generation++;
    clearTimeout(timeout);
    controller?.abort();
    controller = undefined;
  }

  function updateAutofill() {
    updateIntent();
    autofill.disabled = pending || disposed || !dialog.open || select.disabled ||
      project.value !== loadedProject || !sources.has(select.value);
  }

  function copyIssue() {
    updateAutofill();
    if (autofill.disabled) return;
    const issue = sources.get(select.value);
    title.value = issue.title;
    description.value = issue.body;
    status.textContent = 'Title and description copied. You can edit both fields.';
  }

  function clear() {
    sources.clear();
    loadedProject = undefined;
    autofill.disabled = true;
    select.replaceChildren(new Option('No linked issue', '', true, true));
    select.disabled = true;
  }

  async function load() {
    if (disposed || pending) return;
    clearTimeout(resetTimer);
    cancel();
    clear();
    const projectID = project.value;
    if (!projectID) {
      status.textContent = 'Choose a project to load issues. You can submit without linking an issue.';
      return;
    }
    status.textContent = 'Loading issues… You can submit without linking an issue.';
    const request = new AbortController();
    controller = request;
    const current = generation;
    const isCurrent = () => !disposed && current === generation && project.value === projectID;
    const fail = message => {
      clear();
      status.textContent = message;
    };
    timeout = setTimeout(() => {
      if (!isCurrent()) return;
      cancel();
      fail('Loading issues timed out. Refresh issues to retry, or submit without linking an issue.');
    }, 25000);
    try {
      const response = await fetch(`/projects/issues?project_id=${encodeURIComponent(projectID)}`, {
        signal: request.signal,
      });
      if (!isCurrent()) return;
      if (!response.ok) throw new Error('Issue request failed');
      const data = await response.json();
      if (!isCurrent()) return;
      if (!Array.isArray(data.issues) || data.issues.some(issue =>
        !issue || !Number.isInteger(issue.number) || issue.number < 1 ||
        typeof issue.body !== 'string' || typeof issue.title !== 'string' || typeof issue.url !== 'string' || !issue.url
      )) throw new Error('Invalid issue response');
      for (const issue of data.issues) {
        sources.set(issue.url, {title: issue.title, body: issue.body});
        select.add(new Option(`#${issue.number} ${issue.title}`, issue.url));
      }
      loadedProject = projectID;
      select.disabled = data.issues.length === 0;
      updateAutofill();
      status.textContent = data.issues.length
        ? 'Choose an issue, or leave No linked issue selected.'
        : 'No open issues found. You can submit without linking an issue. Refresh issues to check again.';
    } catch {
      if (isCurrent()) {
        fail('Unable to load issues. Refresh issues to retry, or submit without linking an issue.');
      }
    } finally {
      if (current === generation) {
        clearTimeout(timeout);
        controller = undefined;
      }
    }
  }

  function onReset() {
    cancel();
    clear();
    resetIntent();
    status.textContent = '';
    clearTimeout(resetTimer);
    // The reset event fires before the browser restores default project values.
    resetTimer = setTimeout(() => { resetIntent(); load(); }, 0);
  }

  const observer = new MutationObserver(() => {
    if (dialog.open) load();
    else {
      clearTimeout(resetTimer);
      cancel();
      clear();
      status.textContent = '';
    }
  });
  observer.observe(dialog, { attributes: true, attributeFilter: ['open'] });
  select.addEventListener('change', updateAutofill);
  autofill.addEventListener('click', copyIssue);
  project.addEventListener('change', projectChanged);
  refresh.addEventListener('click', load);
  form.addEventListener('reset', onReset);
  clear();
  if (dialog.open) load();

  cleanup(() => {
    disposed = true;
    cancel();
    clearTimeout(resetTimer);
    observer.disconnect();
    select.removeEventListener('change', updateAutofill);
    autofill.removeEventListener('click', copyIssue);
    project.removeEventListener('change', projectChanged);
    pendingFields.forEach(field => field.remove());
    recoveryObserver.disconnect();
    host.removeEventListener('change', recoveryChanged);
    document.removeEventListener('datastar-fetch', fetchState);
    refresh.removeEventListener('click', load);
    form.removeEventListener('reset', onReset);
    clear();
  });
}
