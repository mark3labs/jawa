// Datastar owns form requests and DOM patches. Rocket (Datastar's web component
// API, bundled with Datastar in datastar-rocket.js) owns the behaviors below.
// Components render in the light DOM so server-rendered markup stays the source
// of truth and live SSE morphs keep working.
// Datastar evaluates existing effects during module initialization, so their
// global helpers must be registered by an earlier dependency.
import './acknowledgement.js';
import {rocket, mergePaths} from './datastar-rocket.js';
import Sortable from 'sortablejs';
import {setupAgentSelector} from './agent-selector.js';
import {setupIssueSelector} from './issue-selector.js';

rocket('jawa-agent-selector', {mode: 'light', setup: ({host, cleanup}) => setupAgentSelector(host, cleanup)});

// Installation is progressive enhancement; HTTP deployments remain usable.
if ('serviceWorker' in navigator && window.isSecureContext) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register('/sw.js', {scope: '/', updateViaCache: 'none'})
      .catch(() => { /* Offline or disabled by browser policy; normal UI still works. */ });
  }, {once: true});
}

// Load project issues only when needed; keep card drafts outside live patches.
rocket('jawa-issue-selector', {
  mode: 'light',
  setup: ({host, cleanup}) => setupIssueSelector(host, cleanup),
});

// Native reset happens after its event; synchronize browser-owned controls once
// defaults have been restored. Delegation also covers inserted SSE fragments.
document.addEventListener('reset', event => {
  if (!(event.target instanceof HTMLFormElement)) return;
  queueMicrotask(() => {
    for (const root of event.target.querySelectorAll('[data-slot="checkbox"], [data-slot="switch"]')) {
      const input = root.nextElementSibling;
      if (!input?.matches('input[type="checkbox"]')) continue;
      if (root.dataset.slot === 'checkbox') window.templ.checkbox.setChecked(root, input.checked);
      else input.dispatchEvent(new Event('change', {bubbles: true}));
    }
  });
});

// Morphs may change checked attributes without firing native change. The
// switch runtime has no setChecked API: its native change adapter is public DOM
// behavior. Observe only native checked mutations, never indicator attributes.
const componentStates = new MutationObserver(records => {
  for (const {target} of records) {
    if (!target.matches('input[type="checkbox"]')) continue;
    const root = target.previousElementSibling;
    if (!root?.matches('[data-slot="checkbox"], [data-slot="switch"]')) continue;
    if (root.dataset.slot === 'checkbox') window.templ.checkbox.setChecked(root, target.checked);
    else target.dispatchEvent(new Event('change', {bubbles: true}));
  }
});
componentStates.observe(document.documentElement, {subtree: true, attributes: true, attributeFilter: ['checked']});

const cardsIn = list => [...list.querySelectorAll(':scope > .task-card')];

// jawa-board: drag-and-drop between lanes. Sortable only handles the gesture;
// the authoritative layout always comes back from the server.
rocket('jawa-board', {
  mode: 'light',
  props: ({string}) => ({project: string}),
  setup: ({host, props, $, cleanup}) => {
    const lists = new Map();
    const board = () => document.getElementById('board-content');
    const move = (cardId, status, position) => {
      const form = document.getElementById('move-form');
      form.elements.card_id.value = cardId;
      form.elements.status.value = status;
      form.elements.position.value = String(position);
      form.requestSubmit();
    };
    const bind = () => {
      for (const [list, sortable] of lists) {
        if (!host.contains(list)) { sortable.destroy(); lists.delete(list); }
      }
      for (const list of host.querySelectorAll('.task-list')) {
        if (lists.has(list)) continue;
        lists.set(list, Sortable.create(list, {
          group: `project-${props.project}`,
          draggable: '.task-card',
          animation: 150,
          delay: 120,
          delayOnTouchOnly: true,
          filter: 'a,button,summary,form,select,input,textarea,[role=checkbox],[role=switch],[popover]',
          preventOnFilter: false,
          ghostClass: 'drag-ghost',
          chosenClass: 'drag-chosen',
          onStart: () => board()?.setAttribute('data-ignore-morph', ''),
          onEnd: ({item, from, to, oldIndex, newIndex}) => {
            // Undo the optimistic move, then let the server decide.
            from.insertBefore(item, cardsIn(from)[oldIndex] ?? null);
            board()?.removeAttribute('data-ignore-morph');
            if (from.dataset.status === 'Done' && to.dataset.status === 'Fix') {
              if (item.dataset.canFix === 'true') {
                $.fixCard = item.dataset.cardId;
                $.fixMessage = '';
                $.fixOpen = true;
              } else host.dispatchEvent(new CustomEvent('board-refresh', {bubbles: true}));
            } else if (from.dataset.status === 'Planning' && to.dataset.status === 'Building') {
              // Approval requires an explicit revision and fresh backend checks.
              window.location.assign(`/planning?card=${encodeURIComponent(item.dataset.cardId)}#approve-plan`);
            } else if (from !== to || oldIndex !== newIndex) move(item.dataset.cardId, to.dataset.status, newIndex);
            else host.dispatchEvent(new CustomEvent('board-refresh', {bubbles: true}));
          },
        }));
      }
    };
    bind();
    const observer = new MutationObserver(bind);
    observer.observe(host, {childList: true, subtree: true});
    cleanup(() => {
      observer.disconnect();
      lists.forEach(sortable => sortable.destroy());
      lists.clear();
    });
  },
});

// jawa-menu: positions a native popover beside its trigger and closes it once
// an action is chosen. The popover keeps its coordinates across SSE morphs via
// data-preserve-attr="style".
rocket('jawa-menu', {
  mode: 'light',
  setup: ({host, cleanup}) => {
    const popover = host.querySelector('[popover]');
    const trigger = host.querySelector('[popovertarget]');
    if (!popover || !trigger) return;
    const place = () => {
      const t = trigger.getBoundingClientRect();
      const m = popover.getBoundingClientRect();
      const left = Math.max(8, Math.min(window.innerWidth - m.width - 8, t.right - m.width));
      const below = t.bottom + 6;
      const top = below + m.height > window.innerHeight - 8 ? Math.max(8, t.top - m.height - 6) : below;
      popover.style.inset = 'auto';
      popover.style.left = `${left}px`;
      popover.style.top = `${top}px`;
    };
    const onToggle = event => {
      if (event.newState === 'open') {
        place();
        popover.querySelector('.menu-item:not(.disabled)')?.focus({preventScroll: true});
      }
    };
    const onClick = event => {
      const item = event.target.closest('.menu-item');
      if (item && !item.classList.contains('disabled')) popover.hidePopover();
    };
    const onKey = event => {
      const items = [...popover.querySelectorAll('.menu-item:not(.disabled)')];
      const at = items.indexOf(document.activeElement);
      if (event.key === 'ArrowDown') items[(at + 1) % items.length]?.focus();
      else if (event.key === 'ArrowUp') items[(at - 1 + items.length) % items.length]?.focus();
      else return;
      event.preventDefault();
    };
    const close = () => popover.matches(':popover-open') && popover.hidePopover();
    popover.addEventListener('toggle', onToggle);
    popover.addEventListener('click', onClick);
    popover.addEventListener('keydown', onKey);
    window.addEventListener('resize', close);
    // Scrolling a tall touch menu must not dismiss it; only outside scrolls do.
    const onScroll = event => {
      if (!popover.contains(event.target)) close();
    };
    window.addEventListener('scroll', onScroll, true);
    cleanup(() => {
      popover.removeEventListener('toggle', onToggle);
      popover.removeEventListener('click', onClick);
      popover.removeEventListener('keydown', onKey);
      window.removeEventListener('resize', close);
      window.removeEventListener('scroll', onScroll, true);
    });
  },
});

// jawa-time: keeps server-rendered relative text fresh between live patches.
const relative = (then, now = Date.now()) => {
  const seconds = (now - then) / 1000;
  if (seconds < 45) return 'just now';
  if (seconds < 90) return '1m ago';
  if (seconds < 45 * 60) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 90 * 60) return '1h ago';
  if (seconds < 36 * 3600) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
};
rocket('jawa-time', {
  mode: 'light',
  props: ({string}) => ({datetime: string}),
  setup: ({host, props, observeProps, cleanup}) => {
    const tick = () => {
      const then = Date.parse(props.datetime);
      if (Number.isNaN(then)) return;
      host.textContent = relative(then);
      host.title = new Date(then).toLocaleString();
    };
    tick();
    observeProps(tick, 'datetime');
    const timer = setInterval(tick, 15000);
    cleanup(() => clearInterval(timer));
  },
});

// jawa-copy: click anywhere inside to copy the `text` prop.
rocket('jawa-copy', {
  mode: 'light',
  props: ({string}) => ({text: string}),
  setup: ({host, props, cleanup}) => {
    let timer;
    const onClick = async event => {
      event.preventDefault();
      try {
        await navigator.clipboard.writeText(props.text);
      } catch {
        return;
      }
      host.dataset.copied = '';
      const label = host.querySelector('.copy-label');
      if (label) { label.dataset.was ??= label.textContent; label.textContent = 'Copied'; }
      clearTimeout(timer);
      timer = setTimeout(() => {
        delete host.dataset.copied;
        if (label) label.textContent = label.dataset.was;
      }, 1600);
    };
    host.addEventListener('click', onClick);
    cleanup(() => { clearTimeout(timer); host.removeEventListener('click', onClick); });
  },
});

// jawa-shortcuts: Linear-style keys. C new card, / filter, G then B/R/A/S go to.
rocket('jawa-shortcuts', {
  mode: 'light',
  setup: ({$, cleanup}) => {
    const destinations = {b: '/board', r: '/runs', a: '/agents', s: '/settings'};
    let armed = 0;
    const typing = el => el?.closest?.('input,textarea,select,[contenteditable="true"]');
    const onKey = event => {
      if (event.metaKey || event.ctrlKey || event.altKey || event.isComposing) return;
      if (typing(event.target) || document.querySelector('dialog[open]')) return;
      const key = event.key.toLowerCase();
      if (armed && Date.now() - armed < 1200 && destinations[key]) {
        armed = 0;
        event.preventDefault();
        window.location.assign(destinations[key]);
        return;
      }
      armed = 0;
      if (key === 'g') armed = Date.now();
      else if (key === 'c' && document.getElementById('card-dialog')) { event.preventDefault(); $.cardOpen = true; }
      else if (key === '/') {
        const filter = document.getElementById('card-filter');
        if (filter) { event.preventDefault(); filter.focus(); filter.select(); }
      }
    };
    window.addEventListener('keydown', onKey);
    cleanup(() => window.removeEventListener('keydown', onKey));
  },
});

// Refresh state outlives board/form replacements; only finite refresh POSTs own it.
const pendingRefreshes = new Set();
mergePaths([['prRefreshing', false]]);
document.addEventListener('datastar-fetch', event => {
  const {type, el} = event.detail;
  if (!el?.matches?.('form.pr-refresh-form')) return;
  if (type === 'started') pendingRefreshes.add(el);
  else if (type === 'finished') pendingRefreshes.delete(el);
  else return;
  mergePaths([['prRefreshing', pendingRefreshes.size > 0]]);
});

// Submit feedback and form hygiene for every Datastar form request. Dialogs are
// closed by server signals; this only locks the submit button while in flight
// and clears a form once the server reports success (no error notice showing).
document.addEventListener('datastar-fetch', event => {
  const {type, el} = event.detail;
  if (!el?.matches?.('form') || el.matches('.pr-refresh-form')) return;
  const buttons = el.querySelectorAll('button[type="submit"]');
  if (type === 'started') {
    el.dataset.pending = '';
    buttons.forEach(button => { button.disabled = true; });
  } else if (type === 'finished') {
    delete el.dataset.pending;
    buttons.forEach(button => { button.disabled = false; });
    const notice = document.getElementById('notice');
    const failed = notice && !notice.hidden && !notice.classList.contains('info');
    if (!failed && (el.closest('dialog') || el.hasAttribute('data-reset-on-success')) && (el.action !== new URL('/cards', location.href).href || !el.closest('dialog')?.open)) el.reset();
  }
});
