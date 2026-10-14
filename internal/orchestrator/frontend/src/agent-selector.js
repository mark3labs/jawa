// Fresh bounded presence reads; drafts remain outside unsolicited SSE morphs.
export function setupAgentSelector(host, cleanup) {
  const dialog = host.closest('dialog');
  const form = host.closest('form') || host.querySelector('form');
  const select = form.querySelector('[name="target_agent_id"]');
  const status = form.querySelector('[data-agent-status]');
  const refresh = form.querySelector('[data-agent-refresh]');
  const submit = form.querySelector('[data-agent-submit]');
  let generation = 0, controller, timer, loaded = false, disposed = false, pending = false;
  const plans = [...form.querySelectorAll('[data-assignment-plan]')];
  const card = () => form.querySelector('[name="card_id"]').value;
  const mode = () => form.querySelector('[name="mode"]').value;
  function cancel() { generation++; clearTimeout(timer); controller?.abort(); }
  function update() {
    const plan = mode() === 'build' ? plans.find(p => p.dataset.assignmentPlan === card()) : undefined;
    for (const section of plans) {
      for (const field of section.querySelectorAll('input')) {
        field.disabled = section !== plan;
        if (field.type === 'checkbox') {
          const root = field.previousElementSibling;
          root?.setAttribute('aria-disabled', String(field.disabled));
          root?.toggleAttribute('data-disabled', field.disabled);
          if (root) root.tabIndex = field.disabled ? -1 : 0;
        }
      }
    }
    const revision = plan?.querySelector('[name="expected_revision"]');
    const ack = plan?.querySelector('[name="acknowledge"]');
    submit.disabled = pending || !loaded || !select.value || select.selectedOptions[0]?.disabled ||
      (mode() === 'fix' && !form.querySelector('[name="message"]').value.trim()) ||
      (plan && (!revision || !ack?.checked));
    form.querySelector('[data-agent-submit-label]').textContent = plan ? 'Approve & build with agent' :
      mode() === 'plan' ? 'Plan with agent' : mode() === 'fix' ? 'Fix with agent' : 'Build with agent';
  }
  async function load() {
    if (pending || disposed || !dialog.open) return;
    cancel(); loaded = false; update();
    const selected = select.value;
    select.replaceChildren(new Option('Select an agent', ''));
    select.disabled = true;
    status.textContent = 'Loading agents…';
    controller = new AbortController();
    const current = generation;
    const isCurrent = () => !disposed && dialog.open && current === generation;
    timer = setTimeout(() => { if (isCurrent()) { cancel(); status.textContent = 'Agent loading timed out. Refresh to retry.'; } }, 5000);
    try {
      const response = await fetch('/agents/choices', {signal: controller.signal, cache: 'no-store'});
      if (!response.ok) throw new Error('discovery failed');
      const data = await response.json();
      if (!isCurrent()) return;
      if (!Array.isArray(data.agents) || data.agents.some(a => !a || typeof a.id !== 'string' || !a.id || typeof a.provider !== 'string' || typeof a.model !== 'string' || typeof a.availability !== 'string' || typeof a.selectable !== 'boolean')) throw new Error('invalid choices');
      for (const agent of data.agents) {
        const option = new Option(`${agent.id} · ${agent.provider} / ${agent.model} · ${agent.availability}${agent.selectable ? '' : ' · Cannot accept tasks'}`, agent.id);
        option.disabled = !agent.selectable;
        select.add(option);
      }
      if ([...select.options].some(o => o.value === selected && !o.disabled)) select.value = selected;
      select.disabled = false; loaded = true;
      status.textContent = data.agents.length ? 'Busy agents can be selected; work may wait.' : 'No connected agents. Refresh to check again.';
    } catch {
      if (isCurrent()) status.textContent = 'Unable to load agents. Refresh to retry.';
    } finally {
      if (current === generation) { clearTimeout(timer); update(); }
    }
  }
  function opened() {
    cancel(); loaded = false; select.value = '';
    for (const section of plans) {
      const input = section.querySelector('[name="acknowledge"]');
      if (input) {
        input.checked = false;
        window.templ?.checkbox?.setChecked(input.previousElementSibling, false);
      }
    }
    update();
    if (dialog.open) load();
  }
  function fetchState(event) {
    if (event.detail.el !== form) return;
    if (event.detail.type === 'started') pending = true;
    else if (event.detail.type === 'finished') pending = false;
    else return;
    refresh.disabled = pending;
    update();
  }
  document.addEventListener('datastar-fetch', fetchState);
  const observer = new MutationObserver(opened);
  observer.observe(dialog, {attributes: true, attributeFilter: ['open']});
  form.addEventListener('input', update);
  form.addEventListener('change', update);
  refresh.addEventListener('click', load);
  cleanup(() => { disposed = true; cancel(); observer.disconnect(); document.removeEventListener('datastar-fetch', fetchState); form.removeEventListener('input', update); form.removeEventListener('change', update); refresh.removeEventListener('click', load); });
}
