import './datastar.js';
import Sortable from 'sortablejs';

// Standard custom-element fallback. Datastar Pro Rocket is licensed and is not
// distributed by this repository; this element can be adapted when supplied.
class JawaBoard extends HTMLElement {
  connectedCallback() {
    if (this.sortables) return;
    this.busy = false;
    this.sortables = [...this.querySelectorAll('.task-list')].map(list => Sortable.create(list, {
      group: `project-${this.getAttribute('project-id')}`, animation: 160,
      handle: '.drag-handle', ghostClass: 'drag-ghost',
      onEnd: e => {
        if (e.from === e.to && e.oldIndex === e.newIndex) return;
        this.move(e.item, e.to, e.newIndex, e.from, e.oldIndex);
      }
    }));
    this.onChange = e => {
      const select = e.target.closest('[data-move-card]');
      if (!select || !select.value) return;
      const card = select.closest('.task-card');
      const from = card.parentElement;
      const old = [...from.children].indexOf(card);
      const to = this.querySelector(`.task-list[data-status="${select.value}"]`);
      select.value = '';
      to.append(card);
      this.move(card, to, to.children.length - 1, from, old);
    };
    this.addEventListener('change', this.onChange);
  }
  disconnectedCallback() {
    this.sortables?.forEach(s => s.destroy());
    this.sortables = null;
    this.removeEventListener('change', this.onChange);
  }
  async move(card, to, position, from, oldIndex) {
    const revert = () => from.insertBefore(card, from.children[oldIndex] || null);
    if (this.busy) { revert(); return; }
    this.busy = true;
    this.sortables.forEach(s => s.option('disabled', true));
    this.querySelectorAll('select').forEach(s => s.disabled = true);
    card.classList.add('saving');
    try {
      const response = await fetch('/cards/move', {
        method: 'POST', headers: {'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': this.getAttribute('csrf')},
        body: new URLSearchParams({card_id: card.dataset.cardId, status: to.dataset.status, position: String(position)})
      });
      if (!response.ok || response.redirected && new URL(response.url).pathname !== '/') throw new Error('Move rejected. Refresh the board and try again.');
      this.dispatchEvent(new CustomEvent('card-moved', {bubbles: true, composed: true, detail: {cardId: card.dataset.cardId, status: to.dataset.status, position}}));
      location.reload();
    } catch (error) {
      revert();
      const notice = document.querySelector('#notice');
      notice.textContent = error.message; notice.hidden = false;
    } finally {
      card.classList.remove('saving'); this.busy = false;
      this.sortables.forEach(s => s.option('disabled', false));
      this.querySelectorAll('select').forEach(s => s.disabled = false);
    }
  }
}
customElements.define('jawa-board', JawaBoard);
document.addEventListener('click', e => {
  const opener = e.target.closest('[data-open]');
  if (opener) document.getElementById(opener.dataset.open)?.showModal();
  if (e.target.closest('[data-close]')) e.target.closest('dialog')?.close();
});
