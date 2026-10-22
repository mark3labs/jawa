// Datastar owns state and requests; shadcn-templ owns component behavior.
// Sortable reports gestures only; the server decides the authoritative layout.
import Sortable from 'sortablejs';
import './mermaid.js';
const emit = (el, type, detail) => el.dispatchEvent(new CustomEvent(type, {bubbles: true, detail}));
const cardsIn = list => [...list.querySelectorAll(':scope > .task-card')];
window.jawaBoard = host => {
  const lists = new Map();
  const bind = () => {
    for (const [list, sortable] of lists) if (!host.contains(list)) { sortable.destroy(); lists.delete(list); }
    for (const list of host.querySelectorAll('.task-list')) {
      if (lists.has(list)) continue;
      lists.set(list, Sortable.create(list, {
        group: `project-${host.dataset.project}`, draggable: '.task-card', animation: 150, delay: 120, delayOnTouchOnly: true,
        filter: 'a,button,summary,form,select,input,textarea,[role=checkbox],[role=switch],[role=menuitem]', preventOnFilter: false,
        ghostClass: 'drag-ghost', chosenClass: 'drag-chosen', onStart: () => emit(host, 'board-drag', {active: true}),
        onEnd: ({item, from, to, oldIndex, newIndex}) => {
          from.insertBefore(item, cardsIn(from).filter(card => card !== item)[oldIndex] ?? null);
          emit(host, 'board-drag', {active: false});
          const card = item.dataset.cardId;
          if (from.dataset.status === 'Done' && to.dataset.status === 'Fix') emit(host, 'board-fix', {card, canFix: item.dataset.canFix === 'true'});
          else if (from.dataset.status === 'Planning' && to.dataset.status === 'Building') emit(host, 'board-plan', {card});
          else if (from !== to || oldIndex !== newIndex) emit(host, 'board-move', {card, status: to.dataset.status, position: newIndex});
          else emit(host, 'board-refresh');
        },
      }));
    }
  };
  bind(); new MutationObserver(bind).observe(host, {childList: true, subtree: true});
};
if ('serviceWorker' in navigator && window.isSecureContext) addEventListener('load', () => navigator.serviceWorker.register('/sw.js', {scope: '/', updateViaCache: 'none'}).catch(() => {}), {once: true});
