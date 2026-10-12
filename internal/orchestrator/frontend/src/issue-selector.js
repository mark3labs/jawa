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

  const sources = new Map();
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
    autofill.disabled = disposed || !dialog.open || select.disabled ||
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
    if (disposed) return;
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
    status.textContent = '';
    clearTimeout(resetTimer);
    // The reset event fires before the browser restores default project values.
    resetTimer = setTimeout(load, 0);
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
  project.addEventListener('change', load);
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
    project.removeEventListener('change', load);
    refresh.removeEventListener('click', load);
    form.removeEventListener('reset', onReset);
    clear();
  });
}
