const all = (sel, root = document) => [...root.querySelectorAll(sel)];
const toast = document.getElementById('toast');
let toastTimer;

function showToast(msg) {
  if (msg) toast.querySelector('span').textContent = msg;
  toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (toast.hidden = true), 2200);
}

function copy(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text).catch(() => {});
  const area = Object.assign(document.createElement('textarea'), { value: text });
  document.body.append(area);
  area.select();
  document.execCommand('copy');
  area.remove();
}

function openDialog(trigger) {
  const dlg = document.getElementById(trigger.dataset.open);
  for (const el of all('[data-fill]', dlg)) {
    const v = trigger.dataset[el.dataset.fill] || '';
    if (el.tagName === 'INPUT') el.value = v;
    else el.textContent = v;
  }
  for (const el of all('[data-if]', dlg)) el.hidden = !trigger.dataset[el.dataset.if];
  dlg.showModal();
}

document.addEventListener('click', (e) => {
  const t = e.target;
  const c = t.closest('[data-copy]');
  if (c) {
    e.preventDefault();
    copy(c.dataset.copy);
    showToast(c.dataset.msg || 'Copied');
    return;
  }
  const o = t.closest('[data-open]');
  if (o) {
    e.preventDefault();
    openDialog(o);
    return;
  }
  if (t.closest('[data-close]') || t.tagName === 'DIALOG') {
    t.closest('dialog').close();
    return;
  }
  const s = t.closest('[data-seg]');
  if (s) {
    e.preventDefault();
    for (const b of s.parentElement.children) b.classList.toggle('on', b === s);
    const v = s.dataset.seg;
    for (const el of all('[data-show]', s.closest('[data-scope]'))) {
      el.hidden = v !== 'all' && !el.dataset.show.split(' ').includes(v);
    }
  }
});

document.addEventListener('submit', (e) => {
  const b = e.submitter;
  if (b && b.hasAttribute('data-busy')) b.classList.add('busy');
});

for (const input of all('[data-filter]')) {
  const list = document.getElementById(input.dataset.filter);
  input.addEventListener('input', () => {
    const q = input.value.trim().toLowerCase();
    let shown = 0;
    for (const el of all('[data-key]', list)) {
      el.hidden = !el.dataset.key.toLowerCase().includes(q);
      if (!el.hidden) shown++;
    }
    const empty = list.querySelector('[data-empty]');
    if (empty && q) {
      empty.hidden = shown > 0;
      const span = empty.querySelector('[data-q]');
      if (span) span.textContent = input.value.trim();
    }
  });
}

for (const bars of all('.bars')) {
  const hint = bars.parentElement.querySelector('.hint');
  bars.addEventListener('mouseover', (e) => {
    const b = e.target.closest('[data-label]');
    if (b) hint.textContent = b.dataset.label;
  });
  bars.addEventListener('mouseleave', () => (hint.textContent = hint.dataset.idle));
}

document.addEventListener('keydown', (e) => {
  if (e.key !== '/' || /INPUT|TEXTAREA|SELECT/.test(document.activeElement.tagName)) return;
  const q = document.getElementById('q');
  if (q) {
    e.preventDefault();
    q.focus();
  }
});

for (const d of all('dialog[data-auto]')) d.showModal();
if (toast && !toast.hidden) showToast();
