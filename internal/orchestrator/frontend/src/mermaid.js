import mermaid from 'mermaid';

// Card Markdown only: Runs deliberately retain literal code and copy controls.
const scope = '#planning-content .planning-markdown, #dialog-slot .planning-markdown';
const states = new WeakMap();
const cache = new Map();
let serial = 0, scheduled = false, running = false, dirty = false;
const tokens = getComputedStyle(document.documentElement);
const color = name => tokens.getPropertyValue(name).trim();
mermaid.initialize({
  startOnLoad: false, securityLevel: 'strict', htmlLabels: false,
  maxTextSize: 50000, maxEdges: 500, suppressErrorRendering: true,
  theme: 'base', themeVariables: {
    darkMode: true, background: color('--surface-2'),
    primaryColor: color('--surface-3'), primaryTextColor: color('--text'),
    primaryBorderColor: color('--amber'), lineColor: color('--text-2'),
    secondaryColor: color('--surface-2'), tertiaryColor: color('--bg-sidebar'),
    fontFamily: 'Inter, sans-serif',
  },
  secure: ['secure', 'securityLevel', 'startOnLoad', 'htmlLabels', 'flowchart',
    'maxTextSize', 'maxEdges', 'suppressErrorRendering', 'theme', 'themeVariables'],
});

// Reject configuration and resource-bearing syntax BEFORE Mermaid touches the DOM.
// Unsupported/unsafe constructs intentionally retain source, not partial output.
function permitted(source) {
  return source.length <= 50000 &&
    !/^\s*---|%%\s*\{|<\s*[a-z!/?]|[\\&]|!\[|#(?:\d|x[0-9a-f])|(?:https?:|data:|javascript:|\/\/)|\b(?:click|img|image|icon|style|classDef|linkStyle)\b|url\s*\(/im.test(source);
}
function remember(source, result) {
  cache.delete(source);
  cache.set(source, result);
  if (cache.size > 64) cache.delete(cache.keys().next().value);
}
function eligible(code) {
  if (code.closest(scope)) return true;
  // Dialog runtime portals to body, retaining the declaration's owner.
  const portal = code.closest('[data-base-ui-portal]');
  return code.closest('.planning-markdown') && portal?._templPortalOwner?.closest('#dialog-slot');
}
function current(code, source) {
  return code.isConnected && code.matches('pre > code.language-mermaid') &&
    eligible(code) && code.textContent === source;
}
function output(code, source, result, open) {
  const pre = code.parentElement;
  const figure = document.createElement('figure');
  figure.className = 'planning-diagram';
  figure.dataset.diagramOutput = '';
  let details;
  if (result) {
    // A cache entry has its own prefix; every placement gets a fresh namespace.
    const id = `jawa-diagram-${++serial}`;
    const viewport = document.createElement('div');
    viewport.className = 'planning-diagram-viewport';
    viewport.setAttribute('role', 'img');
    viewport.setAttribute('aria-label', 'Mermaid diagram');
    const parsed = new DOMParser().parseFromString(result.svg, 'image/svg+xml');
    const ids = [...parsed.querySelectorAll('[id]')].map(el => el.id);
    const names = new Map(ids.map((old, index) => [old, `${id}-${index}`]));
    // Rename only definitions and references, never user-visible label text.
    const escaped = ids.sort((a, b) => b.length - a.length)
      .map(value => value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|');
    const references = new RegExp(`#(${escaped})(?![\\w-])`, 'g');
    const rewrite = value => value.replace(references, (_, old) => `#${names.get(old)}`);
    for (const el of parsed.querySelectorAll('*')) {
      if (el.id) el.id = names.get(el.id);
      for (const attr of [...el.attributes]) {
        if (attr.name === 'id') continue;
        attr.value = attr.name.startsWith('aria-')
          ? attr.value.split(' ').map(value => names.get(value) || value).join(' ')
          : rewrite(attr.value);
      }
      if (el.localName === 'style') el.textContent = rewrite(el.textContent);
    }
    viewport.append(document.importNode(parsed.documentElement, true));
    figure.append(viewport);
    details = document.createElement('details');
    details.open = open;
    const summary = document.createElement('summary');
    summary.textContent = 'Diagram source';
    details.append(summary, pre.cloneNode(true));
    details.querySelector('pre').hidden = false;
    figure.append(details);
    pre.hidden = true;
  } else {
    const status = document.createElement('p');
    status.setAttribute('role', 'status');
    status.textContent = 'Diagram unavailable; source shown above.';
    figure.append(status);
    pre.hidden = false;
  }
  pre.after(figure);
  states.set(code, {source, figure, details});
}
async function render(source) {
  if (cache.has(source)) return cache.get(source);
  let result = null;
  if (permitted(source)) {
    const id = `jawa-render-${++serial}`;
    const host = document.createElement('div');
    host.dataset.diagramOutput = '';
    host.className = 'planning-diagram-staging';
    document.body.append(host);
    try {
      const rendered = await mermaid.render(id, source, host);
      // Strict Mermaid sanitization is the SVG boundary. Never bind callbacks.
      // Defense in depth: final output must not carry resource URLs or HTML.
      const doc = new DOMParser().parseFromString(rendered.svg, 'image/svg+xml');
      const unsafe = doc.querySelector('parsererror, script, foreignObject, image, a') ||
        [...doc.querySelectorAll('*')].some(el => [...el.attributes].some(attr =>
          /^on/i.test(attr.name) || (/href$/i.test(attr.name) && !attr.value.startsWith('#')))) ||
        /@import|url\(\s*['"]?(?!#)/i.test(rendered.svg);
      if (!unsafe) result = {id, svg: rendered.svg};
    } catch {
      // Do not log source or insert Mermaid's global error diagram.
    } finally {
      host.remove();
    }
  }
  remember(source, result);
  return result;
}
async function scan() {
  scheduled = false;
  if (running) { dirty = true; return; }
  running = true;
  try {
    for (const code of document.querySelectorAll('.planning-markdown pre > code.language-mermaid')) {
      if (!eligible(code) || code.closest('[data-diagram-output]')) continue;
      const source = code.textContent;
      if (!current(code, source)) continue;
      const previous = states.get(code);
      if (previous?.source === source && previous.figure.isConnected) {
        code.parentElement.hidden = !!previous.figure.querySelector('svg');
        continue;
      }
      // A detached figure retains its disclosure. Read the native property now,
      // not a queued toggle event, and never share state through the SVG cache.
      const open = previous?.source === source && !!previous.details?.open;
      previous?.figure.remove();
      code.parentElement.hidden = false;
      const result = await render(source);
      if (current(code, source)) output(code, source, result, open);
    }
  } finally {
    running = false;
    if (dirty) { dirty = false; schedule(); }
  }
}
function schedule() {
  if (!scheduled) { scheduled = true; queueMicrotask(scan); }
}
new MutationObserver(records => {
  if (records.some(r => !r.target.closest?.('[data-diagram-output]'))) schedule();
}).observe(document.body, {childList: true, characterData: true, subtree: true});
schedule();
