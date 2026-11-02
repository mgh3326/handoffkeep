// Local preview interactions only. No fetches, writes, storage, or dependencies.
const app = document.querySelector('.app');
const backdrop = document.querySelector('[data-palette]');
const dialog = backdrop.querySelector('[role="dialog"]');
const search = dialog.querySelector('input');
const results = [...dialog.querySelectorAll('.palette-result')];
let previousFocus;
let selected = 0;
let goAt = 0;

function setTheme(theme) {
  document.documentElement.dataset.theme = theme;
  const toggle = document.querySelector('[data-theme-toggle]');
  toggle.textContent = theme === 'dark' ? 'Light' : 'Dark';
  toggle.setAttribute('aria-label', `Switch to ${theme === 'dark' ? 'light' : 'dark'} theme`);
}
setTheme(new URLSearchParams(location.search).get('theme') === 'light' ? 'light' : 'dark');
document.querySelector('[data-theme-toggle]').addEventListener('click', () => {
  setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
});

function visibleResults() { return results.filter(result => !result.hidden); }
function highlight(index) {
  const visible = visibleResults();
  selected = visible.length ? (index + visible.length) % visible.length : 0;
  results.forEach(result => result.classList.remove('selected'));
  visible[selected]?.classList.add('selected');
}
function openPalette() {
  previousFocus = document.activeElement === document.body ? document.querySelector('[data-open-palette]') : document.activeElement;
  backdrop.hidden = false;
  app.inert = true;
  document.body.style.overflow = 'hidden';
  search.focus();
}
function closePalette() {
  backdrop.hidden = true;
  app.inert = false;
  document.body.style.overflow = '';
  previousFocus?.focus();
}
document.querySelectorAll('[data-open-palette]').forEach(button => {
  button.addEventListener('click', event => { event.preventDefault(); openPalette(); });
});
document.querySelector('[data-close-palette]').addEventListener('click', closePalette);
backdrop.addEventListener('click', event => { if (event.target === backdrop) closePalette(); });
search.addEventListener('input', () => {
  const query = search.value.trim().toLocaleLowerCase();
  results.forEach(result => { result.hidden = !result.textContent.toLocaleLowerCase().includes(query); });
  dialog.querySelector('.palette-empty').hidden = visibleResults().length > 0;
  highlight(0);
});
dialog.addEventListener('keydown', event => {
  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    event.preventDefault(); highlight(selected + (event.key === 'ArrowDown' ? 1 : -1));
    visibleResults()[selected]?.scrollIntoView({ block: 'nearest' });
  }
  if (event.key === 'Enter' && event.target === search) {
    event.preventDefault(); visibleResults()[selected]?.click();
  }
  if (event.key === 'Tab') {
    const stops = [...dialog.querySelectorAll('input, button, a[href]')].filter(node => !node.hidden && node.getClientRects().length);
    const first = stops[0], last = stops.at(-1);
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
    if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
  }
});

const menu = document.querySelector('[data-menu]');
menu.addEventListener('click', () => {
  const open = app.classList.toggle('menu-open');
  menu.setAttribute('aria-expanded', String(open));
});

// Preserve the chosen preview theme across local links, including file://.
document.addEventListener('click', event => {
  const link = event.target.closest('a[href]');
  if (!link || link.hasAttribute('data-open-palette') || link.getAttribute('href').startsWith('#')) return;
  const url = new URL(link.href);
  if (!url.pathname.endsWith('.html')) return;
  url.searchParams.set('theme', document.documentElement.dataset.theme);
  link.href = url.href;
});

const mine = new URLSearchParams(location.search).get('work') === 'mine';
if (mine) {
  document.querySelectorAll('.task-row:not([data-mine])').forEach(row => { row.hidden = true; row.style.display = 'none'; });
  document.querySelectorAll('.task-group').forEach(group => {
    const count = group.querySelectorAll('.task-row[data-mine]').length;
    group.hidden = count === 0;
    group.querySelector('.group-count').textContent = count;
  });
  document.querySelector('[data-row-count]').textContent = '4 tasks · illustrative assignment';
  document.querySelector('.tabs .active')?.classList.remove('active');
  document.querySelector('[data-my-work]')?.classList.add('active');
}

document.querySelector('[data-decision-preview]')?.addEventListener('submit', event => {
  event.preventDefault();
  const answer = new FormData(event.currentTarget).get('answer');
  document.querySelector('[data-answer-status]').textContent = `Preview: ${answer}. Nothing was sent.`;
});
function openLinkedDisclosure() {
  const id = location.hash.slice(1);
  if (id === 'report-sample' || id === 'other-requests') {
    const detail = document.getElementById(id);
    if (detail) detail.open = true;
  }
}
window.addEventListener('hashchange', openLinkedDisclosure);
openLinkedDisclosure();

document.addEventListener('keydown', event => {
  const key = event.key.toLowerCase();
  if ((event.metaKey || event.ctrlKey) && key === 'k') {
    event.preventDefault(); backdrop.hidden ? openPalette() : closePalette(); return;
  }
  if (key === 'escape') {
    event.preventDefault();
    if (!backdrop.hidden) closePalette();
    else if (app.classList.contains('menu-open')) { app.classList.remove('menu-open'); menu.setAttribute('aria-expanded', 'false'); menu.focus(); }
    else if (document.body.dataset.page === 'Task detail') location.href = `board.html?theme=${document.documentElement.dataset.theme}`;
    return;
  }
  if (!backdrop.hidden || event.isComposing || event.metaKey || event.ctrlKey || event.altKey || event.target.closest('input, textarea, select, [contenteditable]')) return;
  if (key === 'g') { goAt = performance.now(); return; }
  if (goAt && performance.now() - goAt < 900) {
    goAt = 0;
    const destinations = { i: 'decisions.html', b: 'board.html', t: 'board.html#all-tasks', d: 'decisions.html', e: 'task-detail.html#pull-request', h: 'fleet-hosts.html', s: 'command-palette.html' };
    if (destinations[key]) {
      event.preventDefault();
      const url = new URL(destinations[key], location.href);
      url.searchParams.set('theme', document.documentElement.dataset.theme);
      location.href = url.href;
      return;
    }
  }
  if (key === 'j' || key === 'k') {
    const rows = [...document.querySelectorAll('[data-row]')].filter(row => row.getClientRects().length);
    if (!rows.length) return;
    event.preventDefault();
    const current = rows.indexOf(document.activeElement);
    const next = current < 0 ? (key === 'j' ? 0 : rows.length - 1) : (current + (key === 'j' ? 1 : -1) + rows.length) % rows.length;
    rows[next].focus();
  }
});
if (!backdrop.hidden) openPalette();
