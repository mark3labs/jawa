// View-local behavior; the server remains authoritative for eligibility.
export function setupRunDetail(host, mergePaths, cleanup) {
  const draft = host.querySelector('#run-draft');
  const form = host.querySelector('#run-composer');
  let pending = false;
  let submitted = '';
  let confirmed = '';
  const onSignals = event => { if (typeof event.detail.runConfirmedDraft === 'string') confirmed = event.detail.runConfirmedDraft; };
  document.addEventListener('datastar-signal-patch', onSignals);
  let following = true;
  let scrollTop = 0;
  let frame;
  const transcript = () => host.querySelector('#run-transcript');
  const resize = () => {
    if (!draft) return;
    draft.style.height = 'auto';
    draft.style.height = `${Math.min(draft.scrollHeight, 200)}px`;
  };
  const onKey = event => {
    if (event.key !== 'Enter' || !event.ctrlKey || event.isComposing) return;
    event.preventDefault();
    const send = host.querySelector('button[form="run-composer"]');
    if (!pending && send && !send.disabled) form.requestSubmit(send);
  };
  const onSubmit = event => {
    if (event.target !== form) return;
    if (pending) { event.preventDefault(); event.stopImmediatePropagation(); }
  };
  const onFetch = event => {
    const {type, el} = event.detail;
    if (el !== form) return;
    if (type === 'started') {
      submitted = draft.value;
      confirmed = '';
      mergePaths([['runConfirmedDraft', '']]);
      pending = true;
      mergePaths([['runSending', true]]);
    } else if (type === 'finished') {
      if (confirmed === submitted && draft.value === submitted) {
        draft.value = '';
        draft.dispatchEvent(new Event('input', {bubbles: true}));
      }
      pending = false;
      mergePaths([['runSending', false]]);
      resize();
    }
  };
  const onScroll = event => {
    const el = transcript();
    if (event.target !== el) return;
    scrollTop = el.scrollTop;
    following = el.scrollHeight - el.clientHeight - el.scrollTop < 32;
  };
  const restore = () => {
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => {
      const el = transcript();
      if (!el) return;
      // Instant positioning also respects prefers-reduced-motion.
      el.scrollTop = following ? el.scrollHeight : scrollTop;
    });
  };
  resize();
  restore();
  draft?.addEventListener('input', resize);
  draft?.addEventListener('keydown', onKey);
  host.addEventListener('submit', onSubmit, true);
  host.addEventListener('scroll', onScroll, true);
  document.addEventListener('datastar-fetch', onFetch);
  const observer = new MutationObserver(restore);
  observer.observe(host.querySelector('#run-detail-content'), {childList: true, subtree: true, characterData: true});
  cleanup(() => {
    observer.disconnect();
    cancelAnimationFrame(frame);
    draft?.removeEventListener('input', resize);
    draft?.removeEventListener('keydown', onKey);
    host.removeEventListener('submit', onSubmit, true);
    host.removeEventListener('scroll', onScroll, true);
    document.removeEventListener('datastar-fetch', onFetch);
    document.removeEventListener('datastar-signal-patch', onSignals);
  });
}
