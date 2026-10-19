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

for (const root of all('[data-select]')) {
  const input = root.querySelector('input');
  const btn = root.querySelector('[role=combobox]');
  const list = root.querySelector('[role=listbox]');
  const opts = all('[role=option]', list);
  const selected = () => Math.max(0, opts.findIndex((o) => o.getAttribute('aria-selected') === 'true'));
  const isOpen = () => btn.getAttribute('aria-expanded') === 'true';
  let active = selected();

  const setActive = (i) => {
    active = Math.min(Math.max(i, 0), opts.length - 1);
    opts.forEach((o, j) => o.classList.toggle('active', j === active));
    btn.setAttribute('aria-activedescendant', opts[active].id);
    opts[active].scrollIntoView({ block: 'nearest' });
  };

  const place = () => {
    const r = btn.getBoundingClientRect();
    list.style.minWidth = `${r.width}px`;
    const h = list.offsetHeight;
    const up = innerHeight - r.bottom < h + 8 && r.top > h + 8;
    list.style.left = `${r.left}px`;
    list.style.top = `${up ? r.top - h - 6 : r.bottom + 6}px`;
  };

  const open = () => {
    if (isOpen()) return;
    list.showPopover();
    place();
    btn.setAttribute('aria-expanded', 'true');
    setActive(selected());
  };

  const close = () => {
    if (!isOpen()) return;
    list.hidePopover();
    btn.setAttribute('aria-expanded', 'false');
    btn.removeAttribute('aria-activedescendant');
  };

  const choose = (i) => {
    opts.forEach((o, j) => o.setAttribute('aria-selected', String(j === i)));
    input.value = opts[i].dataset.value;
    btn.querySelector('[data-text]').textContent = opts[i].querySelector('span').textContent;
    close();
    btn.focus();
  };

  btn.addEventListener('click', () => (isOpen() ? close() : open()));
  btn.addEventListener('keyup', (e) => {
    if (e.key === ' ') e.preventDefault();
  });
  btn.addEventListener('keydown', (e) => {
    const k = e.key;
    if (!isOpen()) {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(k)) {
        e.preventDefault();
        open();
      }
      return;
    }
    if (k === 'Tab') return close();
    if (k === 'ArrowDown') setActive(active + 1);
    else if (k === 'ArrowUp') setActive(active - 1);
    else if (k === 'Home') setActive(0);
    else if (k === 'End') setActive(opts.length - 1);
    else if (k === 'Enter' || k === ' ') choose(active);
    else if (k === 'Escape') {
      e.stopPropagation();
      close();
    } else return;
    e.preventDefault();
  });
  list.addEventListener('pointermove', (e) => {
    const o = e.target.closest('[role=option]');
    if (o && opts.indexOf(o) !== active) setActive(opts.indexOf(o));
  });
  list.addEventListener('click', (e) => {
    const o = e.target.closest('[role=option]');
    if (o) choose(opts.indexOf(o));
  });
  document.addEventListener('pointerdown', (e) => {
    if (isOpen() && !root.contains(e.target)) close();
  });
  document.addEventListener('scroll', (e) => {
    if (e.target !== list) close();
  }, true);
  window.addEventListener('resize', close);
  root.closest('dialog')?.addEventListener('close', close);
}

for (const d of all('dialog[data-auto]')) d.showModal();
if (toast && !toast.hidden) showToast();
