import './datastar.js';
import Sortable from 'sortablejs';

// Sortable handles gestures only. Datastar owns every request and DOM update.
class JawaBoard extends HTMLElement {
  connectedCallback() {
    if (this.sortables) return;
    this.sortables = [...this.querySelectorAll('.task-list')].map(list => Sortable.create(list, {
      group: `project-${this.getAttribute('project-id')}`,
      animation: 160,
      handle: '.drag-handle',
      ghostClass: 'drag-ghost',
      onStart: () => document.getElementById('board-content').setAttribute('data-ignore-morph', ''),
      onEnd: event => {
        const {item, from, to, oldIndex, newIndex} = event;
        const status = to.dataset.status;
        // Keep the authoritative layout until the server accepts the move.
        from.insertBefore(item, from.children[oldIndex] || null);
        document.getElementById('board-content')?.removeAttribute('data-ignore-morph');
        if (from !== to || oldIndex !== newIndex) this.move(item.dataset.cardId, status, newIndex);
        else this.dispatchEvent(new CustomEvent('board-refresh', {bubbles: true}));
      }
    }));
    this.onChange = event => {
      const select = event.target.closest('[data-move-card]');
      if (!select?.value) return;
      const status = select.value;
      select.value = '';
      const target = [...this.querySelectorAll('.task-list')].find(list => list.dataset.status === status);
      this.move(select.dataset.moveCard, status, target.querySelectorAll('.task-card').length);
    };
    this.addEventListener('change', this.onChange);
  }
  move(cardId, status, position) {
    const form = document.getElementById('move-form');
    form.elements.card_id.value = cardId;
    form.elements.status.value = status;
    form.elements.position.value = String(position);
    form.requestSubmit();
  }
  disconnectedCallback() {
    this.sortables?.forEach(sortable => sortable.destroy());
    this.sortables = null;
    this.removeEventListener('change', this.onChange);
  }
}
customElements.define('jawa-board', JawaBoard);

document.addEventListener('click', event => {
  const opener = event.target.closest('[data-open]');
  if (opener) {
    const dialog = document.getElementById(opener.dataset.open);
    if (dialog?.id === 'card-dialog') {
      const select = dialog.querySelector('[name="project_id"]');
      select.replaceChildren(...[...document.querySelectorAll('[data-project-id]')].map(project => {
        const option = document.createElement('option');
        option.value = project.dataset.projectId;
        option.textContent = project.dataset.projectName;
        return option;
      }));
      select.value = opener.dataset.project || select.options[0]?.value || '';
    }
    dialog?.showModal();
  }
  if (event.target.closest('[data-close]')) event.target.closest('dialog')?.close();
});

// Close creation dialogs only after a successful SSE response; failures retain drafts.
// Datastar's fetch lifecycle is transport-level, not a polling loop.
document.addEventListener('datastar-fetch', event => {
  const {type, el} = event.detail;
  if (type === 'started') {
    if (el?.matches('form')) {
      el.dataset.pending = '';
      el.querySelectorAll('button[type="submit"]').forEach(button => button.disabled = true);
    }
  }
  if (type === 'finished') {
    if (el?.matches('form')) {
      delete el.dataset.pending;
      el.querySelectorAll('button[type="submit"]').forEach(button => button.disabled = false);
      const dialog = el.closest('dialog');
      if (dialog && !document.getElementById('notice')?.textContent.trim()) {
        dialog.close();
        el.reset();
      }
    }
  }
});
