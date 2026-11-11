/* Joinery UI glue. The server renders HTML; this file only handles what htmx
   cannot: tabs, tree expansion, context menu, inline cell editing, dialogs. */
(() => {
  'use strict';

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
  const ICONS = { table: 'table', view: 'view', query: 'query', design: 'design', db: 'db', schema: 'schema' };
  const svg = (name) => `<svg class="i" aria-hidden="true"><use href="#i-${name}"/></svg>`;

  htmx.config.historyEnabled = false;
  const root = document.documentElement;
  if (window.runtime || window.wails) root.classList.add('wails');
  if (/Mac/.test(navigator.platform)) root.classList.add('mac');

  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* storage unavailable */ } },
  };

  // ---- toasts --------------------------------------------------------------
  // Errors stay until dismissed (they are often long and worth copying);
  // confirmations fade on their own unless the pointer is resting on them.
  function toast(msg, kind = 'ok') {
    const error = kind === 'error';
    const el = document.createElement('div');
    el.className = 'toast' + (error ? ' error' : '');
    el.setAttribute('role', error ? 'alert' : 'status');
    el.innerHTML = `<span class="t-msg"></span><button type="button" class="t-x" aria-label="알림 닫기">${svg('close')}</button>`;
    $('.t-msg', el).textContent = msg;
    $('.t-x', el).addEventListener('click', () => el.remove());
    const box = $('#toasts');
    box.appendChild(el);
    while (box.children.length > 5) box.firstElementChild.remove();
    if (error) return;
    const arm = () => setTimeout(() => { if (el.matches(':hover')) arm(); else el.remove(); }, 3500);
    arm();
  }
  document.body.addEventListener('toast', (e) => toast(e.detail.msg, e.detail.kind));
  document.body.addEventListener('htmx:responseError', (e) => {
    if (e.detail.xhr.getResponseHeader('HX-Trigger')) return; // server already sent a toast
    toast(`요청 실패 (${e.detail.xhr.status})`, 'error');
  });
  document.body.addEventListener('htmx:sendError', () => toast('요청을 보낼 수 없습니다', 'error'));

  // ---- confirm dialog (window.confirm is unreliable in some webviews) -------
  function confirmDialog(message, okLabel = '확인', mono = false) {
    return new Promise((resolve) => {
      const wrap = document.createElement('div');
      wrap.className = 'overlay';
      wrap.innerHTML = `<div class="dialog" role="alertdialog" aria-modal="true" style="width:min(${mono ? 640 : 400}px,92vw)">
        <div class="dlg-title">확인</div>
        <div style="padding:18px 20px;white-space:pre-wrap;max-height:60vh;overflow:auto;${mono ? 'font-family:var(--mono);font-size:12px;-webkit-user-select:text;user-select:text' : ''}"></div>
        <div class="dlg-foot" style="margin:0;padding:10px 16px 14px;border:0">
          <button class="btn" data-v="0">취소</button><button class="btn primary" data-v="1"></button></div></div>`;
      wrap.children[0].children[1].textContent = message;
      $('.btn.primary', wrap).textContent = okLabel;
      const back = document.activeElement;
      const done = (v) => { wrap.remove(); document.removeEventListener('keydown', onKey, true); back?.focus?.(); resolve(v); };
      const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); done(false); } };
      wrap.addEventListener('click', (e) => { const b = e.target.closest('[data-v]'); if (b) done(b.dataset.v === '1'); });
      document.addEventListener('keydown', onKey, true);
      document.body.appendChild(wrap);
      $('.btn.primary', wrap).focus();
    });
  }

  // ---- status bar ----------------------------------------------------------
  function setStatus(left) { $('#st-left').textContent = left; }

  // ---- tabs ----------------------------------------------------------------
  const tabbar = $('#tabbar');
  const panels = $('#panels');
  const tabs = new Map(); // key -> {tab, panel}
  let queryCount = 0;

  function activate(key) {
    for (const [k, t] of tabs) {
      const on = k === key;
      t.tab.classList.toggle('active', on);
      t.tab.setAttribute('aria-selected', on ? 'true' : 'false');
      t.panel.classList.toggle('active', on);
    }
    const t = tabs.get(key);
    if (t) t.tab.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    updateStatus();
  }
  const activeKey = () => [...tabs.keys()].find((k) => tabs.get(k).tab.classList.contains('active'));

  // Right side of the status bar: what the active tab is showing.
  function updateStatus() {
    const t = tabs.get(activeKey());
    let text = '';
    if (t) {
      const parts = [t.tab.title];
      const form = $('form.dataform', t.panel);
      const res = $('.qresult', t.panel);
      if (form) {
        const n = $$('tbody tr.sel', form).length;
        if (n > 1) parts.push(`${n}행 선택`);
        const info = $('.rowinfo', form)?.textContent.replace(/\s+/g, ' ').trim();
        if (info) parts.push(info);
        if (form.elements.where?.value.trim()) parts.push('필터 적용 중');
      } else if (res) {
        const sum = $('.rsum', res) || $('.rpane:not([hidden]) .pbar > span', res) || $('.okbar', res);
        if (sum) parts.push(sum.textContent.replace(/\s+/g, ' ').trim());
      }
      text = parts.join('  ·  ');
    }
    $('#st-right').textContent = text;
  }

  // A tab is "dirty" while it holds work that closing would throw away: an edited
  // table design, or query text that has not been run (run queries are in the history).
  function setDirty(panel, on) {
    const t = tabs.get(panel?.dataset.key);
    if (t) t.tab.classList.toggle('dirty', !!on);
  }

  async function closeTab(key, force = false) {
    const t = tabs.get(key);
    if (!t) return;
    if (!force && t.tab.classList.contains('dirty')) {
      activate(key);
      const what = $('form.designform', t.panel) ? '저장하지 않은 설계 변경' : '실행하지 않은 쿼리';
      if (!(await confirmDialog(`"${$('.t-title', t.tab).textContent}" 탭에 ${what}이 있습니다.\n닫으면 사라집니다. 닫을까요?`, '닫기'))) return;
      if (!tabs.has(key)) return;
    }
    const keys = [...tabs.keys()];
    const idx = keys.indexOf(key);
    t.tab.remove();
    t.panel.remove();
    tabs.delete(key);
    const rest = [...tabs.keys()];
    if (rest.length) {
      const wasActive = t.tab.classList.contains('active');
      if (wasActive) activate(rest[Math.min(idx, rest.length - 1)]);
    }
    $('#welcome').classList.toggle('gone', tabs.size > 0);
    updateStatus();
  }

  function openTab({ key, title, tip, icon, url }) {
    if (tabs.has(key)) { activate(key); return Promise.resolve(); }
    const tab = document.createElement('div');
    tab.className = 'tab';
    tab.setAttribute('role', 'tab');
    tab.tabIndex = 0;
    tab.title = tip || title;
    tab.innerHTML = `<span class="ico ico-${icon}" style="margin:0">${svg(ICONS[icon] || icon)}</span><span class="t-title"></span>
      <button class="x" aria-label="탭 닫기">${svg('close')}</button>`;
    $('.t-title', tab).textContent = title;
    tab.addEventListener('click', (e) => {
      if (e.target.closest('.x')) closeTab(key); else activate(key);
    });
    tab.addEventListener('auxclick', (e) => { if (e.button === 1) { e.preventDefault(); closeTab(key); } });
    tab.addEventListener('keydown', (e) => {
      if (e.target !== tab) return;
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); activate(key); }
      if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') { e.preventDefault(); cycleTab(e.key === 'ArrowRight' ? 1 : -1, true); }
    });
    tabbar.appendChild(tab);

    const panel = document.createElement('div');
    panel.className = 'panel';
    panel.dataset.key = key;
    panel.innerHTML = '<div class="loading-msg">불러오는 중…</div>';
    panels.appendChild(panel);
    tabs.set(key, { tab, panel });
    $('#welcome').classList.add('gone');
    activate(key);
    return url ? htmx.ajax('GET', url, { target: panel, swap: 'innerHTML' }) : Promise.resolve();
  }

  function closeTabsFor(connId) {
    for (const k of [...tabs.keys()]) if (k.split('|')[1] === connId) closeTab(k, true);
  }
  function cycleTab(step, focus = false) {
    const keys = [...tabs.keys()];
    if (keys.length < 2) return;
    const next = keys[(keys.indexOf(activeKey()) + step + keys.length) % keys.length];
    activate(next);
    if (focus) tabs.get(next).tab.focus();
  }
  // Cmd/Ctrl+W closes the active tab; Ctrl+Tab and Cmd/Ctrl+Alt+arrows switch tabs.
  document.addEventListener('keydown', (e) => {
    const mod = e.metaKey || e.ctrlKey;
    if (mod && !e.shiftKey && !e.altKey && e.key.toLowerCase() === 'w') {
      const k = activeKey();
      if (k && !$('.overlay')) { e.preventDefault(); closeTab(k); }
    } else if (e.ctrlKey && e.key === 'Tab') {
      e.preventDefault(); cycleTab(e.shiftKey ? -1 : 1);
    } else if (mod && e.altKey && (e.key === 'ArrowRight' || e.key === 'ArrowLeft')) {
      e.preventDefault(); cycleTab(e.key === 'ArrowRight' ? 1 : -1);
    }
  });

  const qs = (o) => new URLSearchParams(Object.fromEntries(Object.entries(o).filter(([, v]) => v))).toString();
  const dbLabel = (r) => r.s || r.d;

  function openTable(r, isView) {
    openTab({
      key: ['t', r.c, r.d, r.s, r.t].join('|'), title: `${r.t} @${dbLabel(r)}`,
      tip: `${r.t} @${dbLabel(r)}`, icon: isView ? 'view' : 'table',
      url: '/table/data?' + qs({ c: r.c, d: r.d, s: r.s, t: r.t, v: isView ? '1' : '' }),
    });
  }
  function openDesign(r, isView) {
    openTab({
      key: ['x', r.c, r.d, r.s, r.t].join('|'), title: `구조: ${r.t}`, tip: `${r.t} @${dbLabel(r)} (구조)`,
      icon: 'design', url: '/table/design?' + qs({ c: r.c, d: r.d, s: r.s, t: r.t }),
    });
  }
  function openDesigner(r) {
    openTab({
      key: ['x', r.c, r.d, r.s, r.t].join('|'), title: `설계: ${r.t}`, tip: `${r.t} @${dbLabel(r)} (설계)`,
      icon: 'design', url: '/design/edit?' + qs({ c: r.c, d: r.d, s: r.s, t: r.t }),
    });
  }
  let newTableCount = 0;
  function openNewTable(r) {
    newTableCount += 1;
    openTab({
      key: ['n', r.c, newTableCount].join('|'), title: newTableCount > 1 ? `새 테이블 ${newTableCount}` : '새 테이블',
      tip: `새 테이블 @${dbLabel(r)}`, icon: 'table', url: '/design/new?' + qs({ c: r.c, d: r.d, s: r.s }),
    });
  }
  // Views are read only; real tables open in the editable designer.
  const openStructure = (r, isView) => (isView ? openDesign(r, true) : openDesigner(r));

  function openObjects(r) {
    openTab({
      key: ['o', r.c, r.d, r.s].join('|'), title: `${dbLabel(r)} 오브젝트`, tip: `${dbLabel(r)} — 오브젝트 목록`,
      icon: r.s ? 'schema' : 'db', url: '/objects?' + qs({ c: r.c, d: r.d, s: r.s }),
    });
  }
  // sql: optional text to put into the new editor (e.g. an object definition)
  function openQuery(r, sql = '') {
    queryCount += 1;
    const loaded = openTab({
      key: ['q', r.c, queryCount].join('|'), title: `쿼리 ${queryCount}`, tip: `쿼리 ${queryCount} @${dbLabel(r) || ''}`,
      icon: 'query', url: '/query?' + qs({ c: r.c, d: r.d, s: r.s }),
    });
    if (sql) {
      loaded.then(() => {
        const ta = $('.panel.active textarea.editor');
        if (!ta) return;
        if (ta._ed) { ta._ed.set(sql); ta._ed.focus(); } else ta.value = sql;
        markQuery(ta);
      });
    }
  }

  // Read-only definition of an index / trigger / function / procedure.
  const KIND_ICON = { index: 'index', trigger: 'trigger', function: 'func', procedure: 'proc', table: 'table', view: 'view' };
  function openDef(r, kind, id, name) {
    openTab({
      key: ['d', r.c, r.d, r.s, kind, id].join('|'), title: name, tip: `${name} (정의)`, icon: KIND_ICON[kind] || 'query',
      url: '/objects/def?' + qs({ c: r.c, d: r.d, s: r.s, kind, id, name }),
    });
  }
  const defRef = (row) => ({ ...refOf(row), kind: row.dataset.kind, id: row.dataset.id, name: row.dataset.name });

  // ---- tree ----------------------------------------------------------------
  const tree = $('#tree');
  let selRow = null;

  const refOf = (row) => ({ c: row.dataset.c || '', d: row.dataset.d || '', s: row.dataset.s || '', t: row.dataset.t || '' });
  const nodeOf = (row) => row.parentElement;
  const kidsOf = (row) => $(':scope > .kids', nodeOf(row));

  function connRowOf(row) { return $(':scope > .row', row.closest('#tree > li')); }

  // Sidebar buttons are enabled only when the selection gives them something to act on.
  function updateActions() {
    const conn = selRow && selRow.isConnected ? connRowOf(selRow) : null;
    const r = conn ? refOf(selRow) : null;
    const ok = { conn: !!conn, closed: !!conn && !conn.dataset.live, live: !!conn && !!conn.dataset.live, db: !!r?.d };
    $$('#sb-actions [data-needs]').forEach((b) => { b.disabled = !ok[b.dataset.needs]; });
  }

  function select(row) {
    if (selRow) selRow.classList.remove('sel');
    selRow = row;
    updateActions();
    if (!row) return;
    row.classList.add('sel');
    const conn = connRowOf(row);
    if (row.dataset.node === 'conn') { setStatus(row.dataset.summary); updateTitle(); return; }
    const parts = [conn?.dataset.name, row.dataset.d, row.dataset.s, row.dataset.t].filter(Boolean);
    setStatus(parts.join(' › '));
    updateTitle();
  }

  async function load(row, force = false) {
    const node = nodeOf(row);
    const kids = kidsOf(row);
    const url = row.dataset.children;
    if (!url || (kids.dataset.loaded && !force)) return;
    node.classList.add('loading');
    try {
      await htmx.ajax('GET', url, { target: kids, swap: 'innerHTML' });
    } finally {
      node.classList.remove('loading');
    }
    if (!$('.tree-error', kids)) {
      kids.dataset.loaded = '1';
      if (row.dataset.cat) { // lazily loaded groups show how many objects they hold
        const cnt = $('.cnt', row);
        if (cnt) cnt.textContent = String($$(':scope > li.leaf', kids).length);
      }
      if (row.dataset.node === 'conn' && !row.dataset.live) {
        row.dataset.live = '1';
        nodeOf(row).dataset.live = '1'; // lets the "connected only" filter keep it
        if (!$('.live', row)) row.insertAdjacentHTML('beforeend', '<span class="live" title="연결됨"></span>');
      }
      updateActions();
    } else {
      delete kids.dataset.loaded;
    }
  }

  async function toggle(row, wantOpen) {
    const node = nodeOf(row);
    if (node.classList.contains('leaf')) return;
    const open = wantOpen ?? !node.classList.contains('open');
    if (open) {
      node.classList.add('open');
      await load(row);
    } else {
      node.classList.remove('open');
    }
  }

  async function refreshNode(row) {
    kidsOf(row).innerHTML = '';
    delete kidsOf(row).dataset.loaded;
    nodeOf(row).classList.add('open');
    await load(row, true);
  }

  tree.addEventListener('click', (e) => {
    const row = e.target.closest('.row');
    if (!row) return;
    select(row);
    if (e.target.closest('.tw')) toggle(row);
  });

  // Open what the row stands for: a table, a definition, or expand + list a database.
  async function activateRow(row) {
    const kind = row.dataset.node;
    if (kind === 'table') return openTable(refOf(row), row.dataset.view === '1');
    if (kind === 'obj') { const o = defRef(row); return openDef(o, o.kind, o.id, o.name); }
    await toggle(row);
    if ((kind === 'db' || kind === 'schema') && nodeOf(row).classList.contains('open') && !$('.tree-error', kidsOf(row))) {
      openObjects(refOf(row));
    }
  }
  tree.addEventListener('dblclick', (e) => {
    const row = e.target.closest('.row');
    if (row) activateRow(row);
  });

  // Keyboard: arrows move, Right/Left expand/collapse, Enter opens, Shift+F10 / Menu key opens the menu.
  const visibleRows = () => $$('.row', tree).filter((r) => r.offsetParent !== null);
  tree.addEventListener('focus', () => { if (!selRow?.isConnected) { const f = visibleRows()[0]; if (f) select(f); } });
  tree.addEventListener('keydown', async (e) => {
    if (e.target !== tree || e.metaKey || e.ctrlKey || e.altKey) return;
    const rows = visibleRows();
    if (!rows.length) return;
    const cur = selRow?.isConnected ? selRow : null;
    const i = cur ? rows.indexOf(cur) : -1;
    const go = (row) => { if (row) { select(row); row.scrollIntoView({ block: 'nearest' }); } };
    if (!cur) { // nothing selected yet: any navigation key starts at the top
      if (['ArrowDown', 'ArrowUp', 'ArrowRight', 'ArrowLeft', 'Home', 'End'].includes(e.key)) { e.preventDefault(); go(rows[0]); }
      return;
    }
    const node = nodeOf(cur);
    switch (e.key) {
      case 'ArrowDown': e.preventDefault(); go(rows[Math.min(i + 1, rows.length - 1)]); break;
      case 'ArrowUp': e.preventDefault(); go(rows[Math.max(i - 1, 0)]); break;
      case 'Home': e.preventDefault(); go(rows[0]); break;
      case 'End': e.preventDefault(); go(rows[rows.length - 1]); break;
      case 'ArrowRight':
        if (!cur) break;
        e.preventDefault();
        if (node.classList.contains('leaf')) break;
        if (!node.classList.contains('open')) await toggle(cur, true);
        else go($(':scope > .kids > li > .row', node));
        break;
      case 'ArrowLeft':
        if (!cur) break;
        e.preventDefault();
        if (!node.classList.contains('leaf') && node.classList.contains('open')) toggle(cur, false);
        else { const up = node.parentElement.closest('.node'); if (up) go($(':scope > .row', up)); }
        break;
      case 'Enter': if (cur) { e.preventDefault(); activateRow(cur); } break;
      case 'F10': if (!e.shiftKey) break; // falls through
      case 'ContextMenu':
        if (cur) { e.preventDefault(); const b = cur.getBoundingClientRect(); treeMenu(cur, b.left + 40, b.bottom); }
        break;
    }
  });

  function ancestorRow(row, kinds) {
    let node = nodeOf(row);
    while (node) {
      const r = $(':scope > .row', node);
      if (r && kinds.includes(r.dataset.node)) return r;
      node = node.parentElement.closest('.node');
    }
    return null;
  }

  // ---- context menu --------------------------------------------------------
  const menu = $('#ctxmenu');
  let menuBack = null;
  function hideMenu(refocus = false) {
    if (menu.hidden) return;
    menu.hidden = true;
    menu.innerHTML = '';
    if (refocus) menuBack?.focus?.();
    menuBack = null;
  }
  function showMenu(x, y, items) {
    menuBack = document.activeElement;
    menu.innerHTML = '';
    menu.setAttribute('role', 'menu');
    for (const it of items) {
      if (it === '-') { menu.appendChild(document.createElement('hr')); continue; }
      const b = document.createElement('button');
      b.setAttribute('role', 'menuitem');
      b.textContent = it.label;
      if (it.danger) b.className = 'danger';
      b.addEventListener('click', () => { hideMenu(true); it.fn(); });
      menu.appendChild(b);
    }
    menu.hidden = false;
    const { width, height } = menu.getBoundingClientRect();
    menu.style.left = Math.max(4, Math.min(x, innerWidth - width - 4)) + 'px';
    menu.style.top = Math.max(4, Math.min(y, innerHeight - height - 4)) + 'px';
    $('button', menu)?.focus({ preventScroll: true });
  }
  menu.addEventListener('keydown', (e) => {
    const items = $$('button', menu);
    const i = items.indexOf(document.activeElement);
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      items[(i + (e.key === 'ArrowDown' ? 1 : -1) + items.length) % items.length]?.focus();
    } else if (e.key === 'Home' || e.key === 'End') {
      e.preventDefault();
      items[e.key === 'Home' ? 0 : items.length - 1]?.focus();
    } else if (e.key === 'Tab') {
      e.preventDefault();
    }
  });
  document.addEventListener('click', (e) => { if (!menu.contains(e.target)) hideMenu(); });
  document.addEventListener('scroll', () => hideMenu(), true);
  window.addEventListener('blur', () => hideMenu());

  tree.addEventListener('contextmenu', (e) => {
    const row = e.target.closest('.row');
    if (!row) return;
    e.preventDefault();
    treeMenu(row, e.clientX, e.clientY);
  });
  function treeMenu(row, x, y) {
    select(row);
    const r = refOf(row);
    const kind = row.dataset.node;
    const items = [];
    if (kind === 'conn') {
      const live = !!row.dataset.live;
      items.push(
        { label: live ? '연결 새로고침' : '연결 열기', fn: () => (live ? refreshNode(row) : toggle(row, true)) },
        ...(live ? [{ label: '연결 닫기', fn: () => closeConnection(r.c) }] : []),
        { label: '새 쿼리', fn: () => openQuery(r) }, '-',
        { label: '연결 편집…', fn: () => htmx.ajax('GET', '/conn/edit?id=' + encodeURIComponent(r.c), { target: '#modal', swap: 'innerHTML' }) },
        { label: '연결 삭제…', danger: true, fn: () => deleteConnection(r.c, row.dataset.name) },
      );
    } else if (kind === 'db' || kind === 'schema') {
      items.push(
        { label: '테이블 목록 열기', fn: () => openObjects(r) },
        { label: '새 테이블…', fn: () => openNewTable(r) },
        { label: '새 쿼리', fn: () => openQuery(r) }, '-',
        { label: '백업 (SQL 덤프)…', fn: () => openBackup(r) },
        { label: 'SQL 파일 실행 (복원)…', fn: () => openRestore(r) }, '-',
        { label: '새로고침', fn: () => refreshNode(row) },
      );
    } else if (kind === 'group') {
      const anc = ancestorRow(row, ['db', 'schema']);
      items.push({ label: '테이블 목록 열기', fn: () => openObjects(r) }, { label: '새 테이블…', fn: () => openNewTable(r) });
      if (anc) items.push({ label: '새로고침', fn: () => refreshNode(anc) });
    } else if (kind === 'table') {
      const isView = row.dataset.view === '1';
      const anc = ancestorRow(row, ['db', 'schema']);
      items.push(
        { label: isView ? '뷰 열기' : '테이블 열기', fn: () => openTable(r, isView) },
        { label: isView ? '구조 보기' : '테이블 설계', fn: () => openStructure(r, isView) },
        { label: '새 쿼리', fn: () => openQuery(r) }, '-',
        { label: '이름 복사', fn: () => copyText(r.t) },
      );
      if (anc) items.push({ label: '새로고침', fn: () => refreshNode(anc) });
    } else if (kind === 'obj') {
      const o = defRef(row);
      const anc = ancestorRow(row, ['db', 'schema']);
      items.push(
        { label: '정의 보기', fn: () => openDef(o, o.kind, o.id, o.name) },
        { label: '쿼리 편집기로 열기', fn: () => defToQuery(o, o.kind, o.id) }, '-',
        { label: '이름 복사', fn: () => copyText(o.name) },
      );
      if (anc) items.push({ label: '새로고침', fn: () => refreshNode(anc) });
    }
    showMenu(x, y, items);
  }

  // Fetch a definition and open it in a new query tab.
  async function defToQuery(r, kind, id) {
    try {
      const res = await fetch('/objects/def?' + qs({ c: r.c, d: r.d, s: r.s, kind, id, name: '' }));
      const html = await res.text();
      const doc = new DOMParser().parseFromString(html, 'text/html');
      const pre = $('pre.ddl', doc);
      if (!pre) return toast((doc.body.textContent || '정의를 불러오지 못했습니다').trim(), 'error');
      openQuery(r, pre.textContent);
    } catch { toast('요청을 보낼 수 없습니다', 'error'); }
  }
  document.addEventListener('contextmenu', (e) => { if (!e.target.closest('input, textarea, .cm-editor')) e.preventDefault(); });

  function ajaxQuiet(method, url, values) {
    return htmx.ajax(method, url, { source: document.body, swap: 'none', values });
  }
  function closeConnection(c) { ajaxQuiet('POST', '/conn/close', { c }); }
  async function deleteConnection(c, name) {
    if (!(await confirmDialog(`연결 "${name}"을(를) 삭제할까요?\n저장된 비밀번호도 함께 삭제됩니다.`, '삭제'))) return;
    closeTabsFor(c);
    ajaxQuiet('POST', '/conn/delete', { c });
  }

  // ---- tree filter ---------------------------------------------------------
  $('#tree-filter').addEventListener('input', (e) => {
    const q = e.target.value.trim().toLowerCase();
    const walk = (li) => {
      const row = $(':scope > .row', li);
      const own = !q || (row?.textContent || '').toLowerCase().includes(q);
      const kids = $$(':scope > .kids > li', li);
      const childHit = kids.map(walk).some(Boolean);
      const show = own || childHit;
      li.hidden = !show;
      if (q && childHit && li.classList.contains('node')) li.classList.add('open');
      return show;
    };
    $$('#tree > li').forEach(walk);
  });

  // ---- toolbar -------------------------------------------------------------
  const selectedRef = () => (selRow ? refOf(selRow) : null);
  const onAction = (e) => {
    const btn = e.target.closest('[data-action]');
    if (!btn) return;
    const r = selectedRef();
    switch (btn.dataset.action) {
      case 'new-query':
        if (!r || !r.c) return toast('트리에서 연결 또는 데이터베이스를 먼저 선택하세요', 'error');
        openQuery(r); break;
      case 'objects':
        if (!r || !r.d) return toast('트리에서 데이터베이스를 먼저 선택하세요', 'error');
        openObjects(r); break;
      case 'refresh':
        htmx.trigger(document.body, 'conns-changed'); break;
      case 'connect': {
        const conn = selRow && connRowOf(selRow);
        if (!conn) return toast('트리에서 연결을 먼저 선택하세요', 'error');
        toggle(conn, true); break;
      }
      case 'disconnect': {
        const conn = selRow && connRowOf(selRow);
        if (!conn || !conn.dataset.live) return toast('연결된 항목을 선택하세요', 'error');
        closeConnection(conn.dataset.c); break;
      }
      case 'new-table':
        if (!r || !r.d) return toast('트리에서 데이터베이스를 먼저 선택하세요', 'error');
        openNewTable(r); break;
      case 'theme': setTheme(root.dataset.theme === 'dark' ? 'light' : 'dark'); break;
    }
  };
  $('#toolbar').addEventListener('click', onAction);
  $('#sb-actions').addEventListener('click', onAction);

  // ---- theme ---------------------------------------------------------------
  function setTheme(t) {
    root.dataset.theme = t;
    store.set('theme', t);
  }
  // Follow the system setting only while the user has not chosen a theme.
  matchMedia('(prefers-color-scheme: dark)').addEventListener?.('change', (e) => {
    if (!store.get('theme')) root.dataset.theme = e.matches ? 'dark' : 'light';
  });

  // ---- command palette (Cmd/Ctrl+K) ----------------------------------------------
  // Lists the sidebar actions and the theme switch (running one clicks its button, so it
  // behaves exactly like the button), the open tabs, the saved connections and every
  // table the tree has loaded so far.
  (() => {
    const host = $('#palette');
    const mac = /Mac|iPhone|iPad/.test(navigator.platform);
    $$('#cmdk-key, .cmdk-hint').forEach((k) => { k.textContent = mac ? '⌘K' : 'Ctrl K'; });

    const NEEDS = { conn: '연결을 선택하세요', closed: '닫힌 연결을 선택하세요', live: '열린 연결을 선택하세요', db: '데이터베이스를 선택하세요' };
    const iconOf = (el) => $$('svg', el).find((x) => getComputedStyle(x).display !== 'none');
    const commands = () => {
      const out = [...$$('#sb-actions .sbtn'), ...$$('#theme-btn')].map((btn) => ({
        label: btn.getAttribute('aria-label') || btn.title, icon: iconOf(btn), off: btn.disabled,
        hint: btn.disabled ? NEEDS[btn.dataset.needs] : '명령', run: () => btn.click(),
      }));
      const cur = activeKey();
      if (cur) out.push({ label: '현재 탭 닫기', icon: iconOf($('.x', tabs.get(cur).tab)), hint: mac ? '⌘W' : 'Ctrl+W', run: () => closeTab(cur) });
      for (const [k, t] of tabs) {
        if (k !== cur) out.push({ label: $('.t-title', t.tab).textContent, icon: iconOf(t.tab), hint: '열린 탭', run: () => activate(k) });
      }
      for (const row of $$('#tree .row[data-node="table"]')) {
        const r = refOf(row);
        const conn = connRowOf(row);
        out.push({
          label: r.t, icon: iconOf(row), hint: `${row.dataset.view === '1' ? '뷰' : '테이블'} · ${conn?.dataset.name || ''} › ${dbLabel(r)}`,
          run: () => { select(row); openTable(r, row.dataset.view === '1'); },
        });
      }
      for (const row of $$('#tree .row[data-node="conn"]')) {
        if (!row.dataset.live) out.push({ label: row.dataset.name, icon: iconOf(row), hint: '연결 열기', run: () => { select(row); toggle(row, true); } });
      }
      return out;
    };

    const close = () => { host.hidden = true; host.replaceChildren(); };
    const open = () => {
      const all = commands();
      let shown = all;
      let cur = 0;
      host.innerHTML = '<div class="pal" role="dialog" aria-label="명령 팔레트"><div class="pal-input">' + svg('search') +
        '<input type="text" placeholder="명령, 테이블, 열린 탭 검색…" aria-label="명령 검색" spellcheck="false" autocomplete="off"></div>' +
        '<div class="pal-list" role="listbox"></div>' +
        '<div class="pal-foot"><span><kbd>↑</kbd> <kbd>↓</kbd> 이동</span><span><kbd>Enter</kbd> 실행</span><span><kbd>Esc</kbd> 닫기</span></div></div>';
      host.hidden = false;
      const input = $('input', host);
      const list = $('.pal-list', host);
      const run = (c) => { if (!c || c.off) return; close(); setTimeout(c.run, 0); };
      const paint = () => {
        list.replaceChildren();
        if (!shown.length) {
          const none = document.createElement('div');
          none.className = 'pal-empty';
          none.textContent = '일치하는 명령이 없습니다';
          list.appendChild(none);
          return;
        }
        shown.slice(0, 60).forEach((c, i) => {
          const row = document.createElement('div');
          row.className = 'pal-item' + (i === cur ? ' active' : '') + (c.off ? ' off' : '');
          row.setAttribute('role', 'option');
          row.setAttribute('aria-selected', String(i === cur));
          if (c.off) row.setAttribute('aria-disabled', 'true');
          if (c.icon) row.appendChild(c.icon.cloneNode(true));
          const label = document.createElement('span');
          label.className = 'pal-label';
          label.textContent = c.label;
          row.appendChild(label);
          if (c.hint) {
            const hint = document.createElement('span');
            hint.className = 'pal-hint';
            hint.textContent = c.hint;
            row.appendChild(hint);
          }
          row.addEventListener('mousemove', () => { if (cur !== i) { cur = i; paint(); } });
          row.addEventListener('click', () => run(c));
          list.appendChild(row);
        });
        $('.pal-item.active', list)?.scrollIntoView({ block: 'nearest' });
      };
      input.addEventListener('input', () => {
        const q = input.value.trim().toLowerCase();
        shown = all.filter((c) => c.label.toLowerCase().includes(q) || (c.hint || '').toLowerCase().includes(q));
        cur = 0;
        paint();
      });
      input.addEventListener('keydown', (e) => {
        if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
          e.preventDefault();
          const n = Math.min(shown.length, 60);
          if (n) cur = (cur + (e.key === 'ArrowDown' ? 1 : -1) + n) % n;
          paint();
        } else if (e.key === 'Enter') {
          e.preventDefault();
          run(shown[cur]);
        } else if (e.key === 'Escape') {
          e.preventDefault();
          e.stopPropagation();
          close();
        }
      });
      paint();
      input.focus();
    };
    host.addEventListener('mousedown', (e) => { if (e.target === host) close(); });
    $('#cmdk-btn').addEventListener('click', open);
    document.addEventListener('keydown', (e) => {
      if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        if (host.hidden) open(); else close();
      }
    });
  })();

  // ---- navigation pane: only connected items ---------------------------------
  $('#nav-live').addEventListener('click', (e) => {
    const on = tree.classList.toggle('only-live');
    e.currentTarget.classList.toggle('active', on);
    e.currentTarget.setAttribute('aria-pressed', String(on));
  });

  // ---- window title (shown in the desktop window's title row) ----------------
  function updateTitle() {
    const parts = [];
    if (selRow) {
      const conn = connRowOf(selRow);
      if (conn) parts.push(conn.dataset.name);
      for (const k of ['d', 's', 't']) if (selRow.dataset[k] && selRow.dataset.node !== 'conn') parts.push(selRow.dataset[k]);
    }
    const text = parts.length ? `${parts.join(' › ')} — Joinery` : 'Joinery';
    $('#win-title').textContent = parts.join(' › ');
    document.title = text;
  }
  document.addEventListener('keydown', (e) => {
    if (e.key === 'F5') { e.preventDefault(); htmx.trigger(document.body, 'conns-changed'); }
  });
  document.body.addEventListener('htmx:afterSwap', (e) => { if (e.target === tree) select(null); });
  document.body.addEventListener('htmx:afterSettle', updateActions);

  // ---- sidebar resize ------------------------------------------------------
  (() => {
    const saved = parseInt(store.get('side-w') || '', 10);
    if (saved >= 180 && saved <= 640) root.style.setProperty('--side-w', saved + 'px');
    const bar = $('#resizer');
    bar.addEventListener('pointerdown', (e) => {
      bar.setPointerCapture(e.pointerId);
      bar.classList.add('drag');
      const move = (ev) => root.style.setProperty('--side-w', Math.max(180, Math.min(640, ev.clientX)) + 'px');
      const up = () => {
        bar.classList.remove('drag');
        bar.removeEventListener('pointermove', move);
        bar.removeEventListener('pointerup', up);
        store.set('side-w', String(parseInt(getComputedStyle(root).getPropertyValue('--side-w'), 10)));
      };
      bar.addEventListener('pointermove', move);
      bar.addEventListener('pointerup', up);
    });
  })();

  // ---- panels: grids -------------------------------------------------------
  const ctxOf = (el) => {
    const f = el.closest('[data-c]');
    return { c: f.dataset.c, d: f.dataset.d, s: f.dataset.s, t: f.dataset.t };
  };

  panels.addEventListener('click', (e) => {
    const panelOf = e.target.closest('.objects-panel');
    if (panelOf) objectsPanelClick(e, panelOf);
    const tr = e.target.closest('table.grid tbody tr');
    if (tr && !e.target.closest('td.editing')) {
      const table = tr.closest('table');
      const multi = table.classList.contains('editable') && tr.dataset.pk;
      if (multi && (e.metaKey || e.ctrlKey)) {
        tr.classList.toggle('sel');
        table._anchor = tr;
      } else if (multi && e.shiftKey && table._anchor?.isConnected) {
        const rows = $$('tbody tr', table);
        const [a, b] = [rows.indexOf(table._anchor), rows.indexOf(tr)].sort((x, y) => x - y);
        rows.forEach((x, i) => x.classList.toggle('sel', i >= a && i <= b));
        getSelection()?.removeAllRanges();
      } else {
        $$('tr.sel', table).forEach((x) => x.classList.remove('sel'));
        tr.classList.add('sel');
        table._anchor = tr;
      }
      updateStatus();
    }
    const rtab = e.target.closest('[data-rtab]');
    if (rtab) { // statement result tabs
      const box = rtab.closest('.qresult');
      $$('[data-rtab]', box).forEach((b) => b.classList.toggle('active', b === rtab));
      $$('[data-rpane]', box).forEach((p) => { p.hidden = p.dataset.rpane !== rtab.dataset.rtab; });
    }
    const sub = e.target.closest('[data-sub]');
    if (sub) {
      const p = sub.closest('.design-panel');
      $$('[data-sub]', p).forEach((b) => b.classList.toggle('active', b === sub));
      $$('[data-subpane]', p).forEach((x) => { x.hidden = x.dataset.subpane !== sub.dataset.sub; });
    }
    const act = e.target.closest('[data-act]');
    if (!act) return;
    const objSel = act.closest('.objects-panel')?._sel; // {t, view} of the selected object
    switch (act.dataset.act) {
      case 'open-selected':
        if (!objSel) return toast('목록에서 오브젝트를 선택하세요', 'error');
        openTable({ ...ctxOf(act), t: objSel.t }, objSel.view); break;
      case 'design-selected':
        if (!objSel) return toast('목록에서 오브젝트를 선택하세요', 'error');
        openStructure({ ...ctxOf(act), t: objSel.t }, objSel.view); break;
      case 'new-table': openNewTable(ctxOf(act)); break;
      case 'def-selected':
        if (!objSel || objSel.kind === 'table') return toast('목록에서 오브젝트를 선택하세요', 'error');
        openDef(ctxOf(act), objSel.kind, objSel.id, objSel.name); break;
      case 'def-copy': copyText($('pre.def-body', act.closest('.def-panel')).textContent); break;
      case 'def-query': {
        const p = act.closest('.def-panel');
        openQuery({ c: p.dataset.c, d: p.dataset.d, s: p.dataset.s }, $('pre.def-body', p).textContent); break;
      }
      case 'form-view': toggleFormView(act.closest('form.dataform')); break;
      case 'query-here': openQuery(ctxOf(act)); break;
      case 'backup': openBackup(ctxOf(act)); break;
      case 'restore': openRestore(ctxOf(act)); break;
      case 'delete-row': deleteSelectedRows(act); break;
      case 'clear-where': {
        const f = act.closest('form.dataform');
        f.elements.where.value = '';
        reloadData(f, 1);
        break;
      }
      case 'cancel-query': htmx.trigger(act.closest('.qform'), 'htmx:abort'); break;
      case 'format-sql': {
        const ed = $('textarea.editor', act.closest('.query-panel'))?._ed;
        if (ed) { ed.format(); ed.focus(); } else toast('에디터를 불러오지 못했습니다', 'error');
        break;
      }
      case 'add-field': addDesignRow(act, 'f'); break;
      case 'add-index': addDesignRow(act, 'x'); break;
      case 'add-fk': addDesignRow(act, 'k'); break;
      case 'rm-row': setDirty(act.closest('.panel'), true); act.closest('tr').remove(); break;
      case 'export': exportTable(act.closest('form.dataform'), act.dataset.fmt); break;
      case 'export-result': exportResult(act, act.dataset.fmt); break;
    }
  });

  // Dropping an existing field/index is a checkbox; strike the row through.
  panels.addEventListener('change', (e) => {
    if (e.target.matches('input[name$=".drop"]')) e.target.closest('tr').classList.toggle('dropped', e.target.checked);
    if (e.target.closest('form.designform')) setDirty(e.target.closest('.panel'), true);
    if (e.target.matches('.pgin')) gotoPage(e.target);
  });
  panels.addEventListener('input', (e) => {
    if (e.target.closest('form.designform')) setDirty(e.target.closest('.panel'), true);
  });
  panels.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && e.target.matches('.pgin')) { e.preventDefault(); gotoPage(e.target); }
  });

  // Reload a data grid at a page, keeping its filter, sort and page size.
  function reloadData(form, page) {
    if (page) form.elements.page.value = String(page);
    $('[title="새로고침"]', form)?.click();
  }
  function gotoPage(input) {
    const form = input.closest('form.dataform');
    const max = parseInt(input.dataset.pages || '', 10);
    let n = parseInt(input.value, 10);
    if (!(n >= 1)) n = 1;
    if (max >= 1) n = Math.min(n, max);
    if (String(n) === form.elements.page.value) { input.value = String(n); return; }
    reloadData(form, n);
  }
  // The row count arrives after the grid: derive the page count from it.
  document.body.addEventListener('htmx:afterSwap', (e) => {
    const total = e.target.matches?.('.total') && $('[data-total]', e.target);
    if (total) {
      const input = $('.pgin', e.target.closest('form.dataform'));
      const pages = Math.max(1, Math.ceil(parseInt(total.dataset.total, 10) / parseInt(input.dataset.size, 10)));
      input.dataset.pages = String(pages);
      $('.pgtotal', input.parentElement).textContent = `/ ${pages.toLocaleString()}`;
    }
    const panel = e.target.closest?.('.panel');
    if (panel === e.target) setDirty(panel, false); // the whole tab was re-rendered (e.g. a design was applied)
    updateStatus();
  });

  // ---- designer rows -------------------------------------------------------
  function addDesignRow(btn, kind) {
    const form = btn.closest('form.designform');
    const counter = $(`input[data-count="${kind}"]`, form);
    const idx = parseInt(counter.value, 10);
    const tpl = $(`template[data-tpl="${kind}"]`, form);
    const body = $(`tbody[data-rows="${kind}"]`, form);
    body.insertAdjacentHTML('beforeend', tpl.innerHTML.replaceAll('__I__', String(idx)));
    setDirty(form.closest('.panel'), true);
    counter.value = String(idx + 1);
    $$('[data-sub]', form).find((b) => b.dataset.sub === { f: 'fields', x: 'indexes', k: 'fks' }[kind])?.click();
    $('input:not([type=hidden])', body.lastElementChild)?.focus();
  }

  // Generic confirmation for destructive requests: data-confirm-msg [+ data-confirm-ok].
  document.body.addEventListener('htmx:confirm', async (e) => {
    const elt = e.detail.elt;
    if (!elt.dataset?.confirmMsg) return;
    e.preventDefault();
    if (await confirmDialog(elt.dataset.confirmMsg, elt.dataset.confirmOk || '확인')) e.detail.issueRequest(true);
  });

  // ---- query history: bring an entry back into the editor -------------------
  async function insertHistory(id) {
    const ta = $('.panel.active textarea.editor');
    if (!ta) return toast('쿼리 탭에서 기록을 열어 주세요', 'error');
    let text;
    try {
      const res = await fetch('/history/sql?id=' + encodeURIComponent(id));
      if (!res.ok) return toast('기록을 불러오지 못했습니다', 'error');
      text = await res.text();
    } catch { return toast('요청을 보낼 수 없습니다', 'error'); }
    if (ta._ed) {
      if (ta._ed.get().trim() === '') ta._ed.set(text); else ta._ed.insert('\n' + text);
      ta._ed.focus();
    } else {
      ta.value = (ta.value.trim() ? ta.value + '\n' : '') + text;
    }
    closeModalNow();
  }

  // Show the exact SQL and ask before the designer changes the schema.
  document.body.addEventListener('htmx:confirm', async (e) => {
    const elt = e.detail.elt;
    if (!elt.matches?.('[data-confirm-sql]')) return;
    e.preventDefault();
    const form = elt.closest('form');
    const body = new URLSearchParams(new FormData(form));
    body.set('fmt', 'text');
    let res;
    try {
      res = await fetch('/design/preview', { method: 'POST', headers: { 'HX-Request': 'true' }, body });
    } catch { return toast('요청을 보낼 수 없습니다', 'error'); }
    const text = await res.text();
    if (!res.ok) { $('.design-msg', form).innerHTML = ''; $('.design-msg', form).textContent = text; $('.design-msg', form).className = 'design-msg errbar'; return; }
    $('.design-msg', form).className = 'design-msg';
    $('.design-msg', form).textContent = '';
    if (!text.trim()) return toast('변경 사항이 없습니다');
    if (await confirmDialog('다음 SQL을 실행합니다.\n\n' + text, '적용', true)) e.detail.issueRequest(true);
  });

  // ---- refresh after server-side changes -----------------------------------
  document.body.addEventListener('rows-changed', (e) => {
    const r = e.detail;
    for (const f of $$('form.dataform')) {
      if (f.dataset.c === r.c && f.dataset.d === r.d && f.dataset.s === r.s && f.dataset.t === r.t) $('[title="새로고침"]', f)?.click();
    }
  });
  document.body.addEventListener('schema-changed', (e) => {
    const r = e.detail;
    const same = (el) => el.dataset.c === r.c && el.dataset.d === r.d && (el.dataset.s || '') === (r.s || '');
    const row = $$('#tree .row').find((x) => ['db', 'schema'].includes(x.dataset.node) && same(x));
    if (row && kidsOf(row).dataset.loaded) refreshNode(row);
    for (const p of $$('.objects-panel')) {
      if (same(p)) htmx.ajax('GET', '/objects?' + qs({ c: r.c, d: r.d, s: r.s }), { target: p.closest('.panel'), swap: 'innerHTML' });
    }
  });

  // ---- export --------------------------------------------------------------
  async function saveText(name, text, mime) {
    const app = window.go?.main?.App;
    if (app?.SaveText) {
      try {
        const p = await app.SaveText(name, text);
        if (p) toast('저장했습니다: ' + p);
      } catch (err) { toast(String(err), 'error'); }
      return;
    }
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([text], { type: mime }));
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 10000);
  }

  // Whole table, honouring the current filter and sort; streamed by the server.
  async function exportTable(form, fmt) {
    const q = new URLSearchParams({ c: form.dataset.c, d: form.dataset.d, s: form.dataset.s, t: form.dataset.t, fmt });
    for (const k of ['where', 'sort', 'desc']) if (form.elements[k]?.value) q.set(k, form.elements[k].value);
    return downloadRel('/table/export?' + q);
  }

  // Server-streamed download: the native window saves through a file dialog,
  // a plain browser follows an attachment link.
  async function downloadRel(rel) {
    const app = window.go?.main?.App;
    if (app?.ExportURL) {
      try {
        const p = await app.ExportURL(rel);
        if (p) toast('저장했습니다: ' + p);
      } catch (err) { toast(String(err), 'error'); }
      return;
    }
    const a = document.createElement('a');
    a.href = rel;
    a.download = '';
    document.body.appendChild(a);
    a.click();
    a.remove();
  }

  // ---- backup / restore -----------------------------------------------------
  const openBackup = (r) => htmx.ajax('GET', '/backup/dialog?' + qs({ c: r.c, d: r.d, s: r.s }), { target: '#modal', swap: 'innerHTML' });
  const openRestore = (r) => htmx.ajax('GET', '/backup/restore-dialog?' + qs({ c: r.c, d: r.d, s: r.s }), { target: '#modal', swap: 'innerHTML' });
  document.addEventListener('click', (e) => {
    const b = e.target.closest('[data-bk]');
    if (!b) return;
    for (const cb of $$('.bk-list input[type=checkbox]', b.closest('form'))) cb.checked = b.dataset.bk === 'all';
  });
  document.addEventListener('submit', (e) => {
    const f = e.target.closest('form[data-backup]');
    if (!f) return;
    e.preventDefault();
    const fd = new FormData(f);
    const total = $$('.bk-list input[type=checkbox]', f).length;
    const picked = fd.getAll('t');
    const q = new URLSearchParams({ c: f.dataset.c, d: f.dataset.d, s: f.dataset.s });
    for (const k of ['struct', 'data', 'drop', 'views', 'objs']) if (fd.get(k)) q.set(k, '1');
    if (!q.has('struct') && !q.has('data')) return toast('구조 또는 데이터 중 하나는 선택하세요', 'error');
    if (picked.length === 0) {
      if (!q.has('views') && !q.has('objs')) return toast('백업할 항목을 선택하세요', 'error');
      q.set('nt', '1');
    } else if (picked.length < total) {
      for (const t of picked) q.append('t', t);
    }
    closeModalNow();
    downloadRel('/backup/dump?' + q);
  });

  // Query results are exported from what is shown (at most 1000 rows).
  function exportResult(btn, fmt) {
    const table = btn.closest('.rpane')?.querySelector('table.grid') || $('.qresult table.grid', btn.closest('.query-panel'));
    if (!table) return toast('내보낼 결과가 없습니다', 'error');
    const head = $$('thead th:not(.rn)', table).map((th) => th.textContent.trim());
    const rows = $$('tbody tr', table).filter((tr) => !$('.empty-row', tr)).map((tr) =>
      $$('td:not(.rn)', tr).map((td) => ($('.null', td) ? null : td.textContent)));
    if (fmt === 'csv') {
      const esc = (v) => (v === null ? '' : /[",\r\n]/.test(v) ? '"' + v.replaceAll('"', '""') + '"' : v);
      const text = '﻿' + [head, ...rows].map((r) => r.map(esc).join(',')).join('\n') + '\n';
      return saveText('query-result.csv', text, 'text/csv;charset=utf-8');
    }
    const objs = rows.map((r) => Object.fromEntries(head.map((h, i) => [h, r[i]])));
    return saveText('query-result.json', JSON.stringify(objs, null, 2) + '\n', 'application/json');
  }

  panels.addEventListener('dblclick', (e) => {
    const orow = e.target.closest('.objects-panel tr.orow, .objects-panel .orow-l, .objects-panel .otile');
    if (orow) return openTable({ ...ctxOf(orow), t: orow.dataset.t }, orow.dataset.view === '1');
    const ox = e.target.closest('.objects-panel .orow-x');
    if (ox) return openDef(ctxOf(ox), ox.dataset.kind, ox.dataset.id, ox.dataset.name);
    const td = e.target.closest('table.grid.editable td[data-col]');
    if (td && !td.classList.contains('editing')) return startEdit(td);
    // Read-only grids (views, query results, tables without a key): show the value.
    const ro = e.target.closest('table.grid.resizable:not(.editable) tbody td:not(.rn)');
    if (ro) showCellViewer(ro);
  });

  // Every form here is submitted by htmx. Block native submission so a form that
  // htmx has not processed yet (or an Enter in an input) can never navigate the
  // page or put field values such as passwords into the URL.
  document.addEventListener('submit', (e) => e.preventDefault());

  // Rows are deleted one request at a time; the first failure stops the run so the
  // message names the row that was refused, and the grid reloads either way.
  async function deleteSelectedRows(btn) {
    const form = btn.closest('form.dataform');
    const trs = $$('tbody tr.sel[data-pk]', form);
    if (!trs.length) return toast('삭제할 행을 먼저 선택하세요 (⌘/Ctrl·Shift+클릭으로 여러 행)', 'error');
    const descOf = (tr) => Object.entries(JSON.parse(tr.dataset.pk)).map(([k, v]) => `${k} = ${v}`).join(', ');
    const lines = trs.slice(0, 8).map(descOf);
    if (trs.length > 8) lines.push(`… 외 ${trs.length - 8}개 행`);
    const head = trs.length > 1 ? `선택한 ${trs.length}개 행을 삭제할까요?` : '다음 행을 삭제할까요?';
    if (!(await confirmDialog(`${head}\n${lines.join('\n')}\n\n이 작업은 되돌릴 수 없습니다.`, '삭제'))) return;
    let done = 0;
    for (const tr of trs) {
      const body = new URLSearchParams({ c: form.dataset.c, d: form.dataset.d, s: form.dataset.s, t: form.dataset.t, pk: tr.dataset.pk });
      let res;
      try {
        res = await fetch('/table/delete', { method: 'POST', headers: { 'HX-Request': 'true' }, body });
      } catch { toast('요청을 보낼 수 없습니다', 'error'); break; }
      if (!res.ok) {
        let msg = '삭제하지 못했습니다';
        try { msg = JSON.parse(res.headers.get('HX-Trigger')).toast.msg; } catch { /* keep default */ }
        toast(`${descOf(tr)}: ${msg}`, 'error');
        break;
      }
      done += 1;
    }
    if (done) toast(`${done}개 행을 삭제했습니다`);
    reloadData(form);
  }
  document.body.addEventListener('htmx:afterRequest', (e) => {
    const { pathInfo, successful, elt } = e.detail;
    if (pathInfo?.requestPath === '/table/cell' && elt.matches?.('td')) finishEdit(elt, successful);
  });

  // ---- inline cell editing -------------------------------------------------
  function startEdit(td) {
    const isNull = !!$('.null', td);
    const orig = isNull ? null : td.textContent;
    td._edit = { html: td.innerHTML, orig };
    const input = document.createElement('input');
    input.value = orig ?? '';
    input.dataset.null = isNull ? '1' : '';
    if (isNull) input.placeholder = 'NULL';
    td.classList.add('editing');
    td.textContent = '';
    td.appendChild(input);
    input.focus();
    input.select();
    input.addEventListener('input', () => { input.dataset.null = ''; });
    input.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter') { ev.preventDefault(); commitEdit(td, input); }
      else if (ev.key === 'Escape') { ev.preventDefault(); cancelEdit(td); }
      else if ((ev.metaKey || ev.ctrlKey) && ev.key === '0') {
        ev.preventDefault();
        input.value = '';
        input.dataset.null = '1';
        input.placeholder = 'NULL';
      }
    });
    // Leaving the cell saves, like the form view; only Esc discards.
    input.addEventListener('blur', () => { if (td.classList.contains('editing') && !td._saving && td._edit) commitEdit(td, input); });
  }

  function cancelEdit(td) {
    if (!td._edit) return;
    // Clear state first: removing the focused <input> fires a synchronous blur
    // that would otherwise re-enter this function mid-replacement.
    const { html } = td._edit;
    td._edit = null;
    td._saving = false;
    td.classList.remove('editing', 'saving');
    td.innerHTML = html;
  }

  function commitEdit(td, input) {
    const isNull = input.dataset.null === '1';
    const { orig } = td._edit;
    if ((orig === null && isNull) || (orig !== null && !isNull && input.value === orig)) return cancelEdit(td);
    const form = td.closest('form.dataform');
    td._saving = true;
    td.classList.add('saving');
    htmx.ajax('POST', '/table/cell', {
      source: td, target: td, swap: 'innerHTML',
      values: {
        c: form.dataset.c, d: form.dataset.d, s: form.dataset.s, t: form.dataset.t,
        pk: td.closest('tr').dataset.pk, col: td.dataset.col, value: isNull ? '' : input.value, null: isNull ? '1' : '',
      },
    });
  }

  function finishEdit(td, ok) {
    const edit = td._edit;
    td.classList.remove('editing', 'saving');
    td._saving = false;
    td._edit = null;
    if (!ok && edit) td.innerHTML = edit.html; // revert on failure; success was swapped in
  }

  // ---- design / query panels ----------------------------------------------
  panels.addEventListener('keydown', (e) => {
    const ta = e.target.closest('textarea.editor');
    if (!ta) return;
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault();
      ta.form.requestSubmit();
    } else if (e.key === 'Tab' && !e.shiftKey) {
      e.preventDefault();
      ta.setRangeText('  ', ta.selectionStart, ta.selectionEnd, 'end');
    }
  });
  // Run only the selected text when there is a selection.
  document.body.addEventListener('htmx:configRequest', (e) => {
    const elt = e.detail.elt;
    if (!elt.matches?.('.qform')) return;
    const ta = $('textarea', elt);
    const sel = ta._ed ? ta._ed.selection()
      : (ta.selectionEnd > ta.selectionStart ? ta.value.slice(ta.selectionStart, ta.selectionEnd) : '');
    if (sel) e.detail.parameters.sql = sel;
  });

  // Query text that has been run is in the history; anything else is unsaved work.
  function markQuery(ta) {
    const text = ta.value.trim();
    setDirty(ta.closest('.panel'), text !== '' && text !== (ta._ran ?? ''));
  }
  panels.addEventListener('input', (e) => { if (e.target.matches('textarea.editor')) markQuery(e.target); });
  document.body.addEventListener('htmx:beforeRequest', (e) => {
    const form = e.detail.elt;
    if (form.matches?.('.qform')) {
      const ta = $('textarea', form);
      ta._ran = ta.value.trim();
      markQuery(ta);
      const out = $('.qtime', form);
      const t0 = performance.now();
      out.textContent = '';
      clearInterval(form._tick);
      form._tick = setInterval(() => { out.textContent = `${((performance.now() - t0) / 1000).toFixed(1)}초`; }, 100);
    }
    // Connecting can take a while (SSH, TLS): say so instead of looking frozen.
    const path = e.detail.pathInfo?.requestPath;
    if (path === '/conn/test' || path === '/conn/save') {
      const msg = $('#conn-msg');
      if (msg) msg.innerHTML = `<span class="busy"><i class="spin"></i>${path === '/conn/test' ? '연결 확인 중…' : '저장 중…'}</span>`;
    }
  });
  const stopTick = (e) => { const f = e.detail.elt; if (f.matches?.('.qform')) { clearInterval(f._tick); f._tick = null; } };
  document.body.addEventListener('htmx:afterRequest', stopTick);
  document.body.addEventListener('htmx:xhr:abort', (e) => {
    stopTick(e);
    if (e.detail.elt.matches?.('.qform')) toast('쿼리 실행을 중지했습니다');
  });

  // Replace the plain <textarea> with the CodeMirror editor. The textarea stays
  // (hidden) as the form field, so everything keeps working without editor.js.
  function enhanceEditor(ta) {
    if (!window.DBCatEditor || ta._ed || !ta.form) return;
    const form = ta.form;
    const host = document.createElement('div');
    host.className = 'cm-host';
    ta.after(host);
    ta.hidden = true;
    const ed = window.DBCatEditor.create(host, {
      doc: ta.value, kind: form.dataset.kind, placeholder: ta.placeholder,
      onChange: (v) => { ta.value = v; markQuery(ta); },
      onRun: () => form.requestSubmit(),
      onError: (m) => toast(m, 'error'),
    });
    ta._ed = ed;
    const q = qs({ c: form.elements.c.value, d: form.elements.d.value, s: form.elements.s.value });
    fetch('/query/schema?' + q).then((r) => (r.ok ? r.json() : null)).then((j) => { if (j) ed.setSchema(j.tables); }).catch(() => {});
    ed.focus();
  }
  document.body.addEventListener('htmx:afterSwap', (e) => { $$('textarea.editor', e.target).forEach(enhanceEditor); });

  // ---- clipboard -----------------------------------------------------------
  async function copyText(text) {
    try {
      await navigator.clipboard.writeText(text);
      return toast('복사했습니다');
    } catch { /* not allowed in this webview: fall back below */ }
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;opacity:0';
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { /* ignore */ }
    ta.remove();
    toast(ok ? '복사했습니다' : '복사하지 못했습니다', ok ? 'ok' : 'error');
  }

  // ---- grid: resizable columns ----------------------------------------------
  // Widths are remembered per table for the session, so paging and sorting
  // (which re-render the grid) keep what the user set.
  const colWidths = new Map();
  const gridKey = (t) => {
    const f = t.closest('form.dataform');
    return f ? [f.dataset.c, f.dataset.d, f.dataset.s, f.dataset.t].join('|') : '';
  };

  // Switch to fixed layout with the current widths so one column can change alone.
  function fixLayout(t) {
    if (t.classList.contains('sized')) return;
    const ths = $$('thead th', t);
    const widths = ths.map((th) => th.getBoundingClientRect().width);
    ths.forEach((th, i) => { th.style.width = widths[i] + 'px'; });
    t.classList.add('sized');
    t.style.width = widths.reduce((a, b) => a + b, 0) + 'px';
  }
  function setColWidth(t, th, w) {
    th.style.width = Math.max(40, Math.round(w)) + 'px';
    t.style.width = $$('thead th', t).reduce((s, x) => s + (parseFloat(x.style.width) || x.getBoundingClientRect().width), 0) + 'px';
  }
  function saveWidths(t) {
    const key = gridKey(t);
    if (!key) return;
    const m = colWidths.get(key) || {};
    $$('thead th[data-col]', t).forEach((th) => { if (th.style.width) m[th.dataset.col] = parseFloat(th.style.width); });
    colWidths.set(key, m);
  }
  function autoFit(t, th) {
    fixLayout(t);
    const r = document.createRange();
    r.selectNodeContents(th);
    let w = r.getBoundingClientRect().width + 28; // padding + sort marker
    const rows = t.tBodies[0]?.rows || [];
    for (let i = 0; i < Math.min(rows.length, 300); i++) {
      const c = rows[i].cells[th.cellIndex];
      if (c) w = Math.max(w, c.scrollWidth + 4);
    }
    setColWidth(t, th, Math.min(w, 900));
    saveWidths(t);
  }
  function startResize(e) {
    e.preventDefault();
    e.stopPropagation();
    const handle = e.currentTarget;
    const th = handle.parentElement;
    const t = th.closest('table');
    fixLayout(t);
    const startX = e.clientX;
    const startW = th.getBoundingClientRect().width;
    handle.setPointerCapture(e.pointerId);
    handle.classList.add('drag');
    const move = (ev) => setColWidth(t, th, startW + ev.clientX - startX);
    const up = () => {
      handle.classList.remove('drag');
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', up);
      handle.removeEventListener('pointercancel', up);
      saveWidths(t);
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
  }
  function enhanceGrid(t) {
    if (t._enh) return;
    t._enh = true;
    $$('thead th[data-col]', t).forEach((th) => {
      const h = document.createElement('span');
      h.className = 'colres';
      h.title = '드래그: 너비 조절 · 더블클릭: 내용에 맞춤';
      h.addEventListener('pointerdown', startResize);
      h.addEventListener('click', (e) => e.stopPropagation()); // never trigger the sort of the header
      h.addEventListener('dblclick', (e) => { e.stopPropagation(); autoFit(t, th); });
      th.appendChild(h);
    });
    const saved = colWidths.get(gridKey(t));
    if (saved) {
      fixLayout(t);
      $$('thead th[data-col]', t).forEach((th) => { if (saved[th.dataset.col]) setColWidth(t, th, saved[th.dataset.col]); });
    }
  }
  document.body.addEventListener('htmx:afterSwap', (e) => { $$('table.grid.resizable', e.target).forEach(enhanceGrid); });

  // ---- grid: cell menu and value viewer ------------------------------------
  function cellInfo(td) {
    const th = td.closest('table').tHead?.rows[0].cells[td.cellIndex];
    const isNull = !!$('.null', td);
    return {
      col: th?.dataset.col || th?.textContent.trim() || '',
      type: (th?.title || '').split(' · ')[0],
      isNull,
      text: isNull ? '' : td.textContent,
    };
  }
  function rowObject(tr) {
    const out = {};
    const heads = $$('thead th:not(.rn)', tr.closest('table'));
    $$('td:not(.rn)', tr).forEach((td, i) => { out[heads[i]?.dataset.col || `col${i + 1}`] = $('.null', td) ? null : td.textContent; });
    return out;
  }

  function showCellViewer(td) {
    const { col, type, isNull, text } = cellInfo(td);
    let pretty = null;
    if (/^\s*[[{]/.test(text)) {
      try { pretty = JSON.stringify(JSON.parse(text), null, 2); } catch { /* not JSON */ }
    }
    const wrap = document.createElement('div');
    wrap.className = 'overlay';
    wrap.innerHTML = `<div class="dialog viewer" role="dialog" aria-modal="true">
      <div class="dlg-title"></div><div class="viewer-bar" hidden></div><pre class="viewer-body" tabindex="0"></pre>
      <div class="dlg-foot viewer-foot"><span class="viewer-info"></span>
        <button type="button" class="btn" data-v="copy">복사</button><button type="button" class="btn primary" data-v="close">닫기</button></div></div>`;
    $('.dlg-title', wrap).textContent = type ? `${col} · ${type}` : col;
    const body = $('.viewer-body', wrap);
    let shown = text;
    const render = (t) => { shown = t; body.textContent = isNull ? 'NULL' : t; body.classList.toggle('null', isNull); };
    render(text);
    $('.viewer-info', wrap).textContent = isNull ? 'NULL' : `${[...text].length.toLocaleString()}자 · ${new Blob([text]).size.toLocaleString()}바이트`;
    if (pretty !== null) {
      const bar = $('.viewer-bar', wrap);
      bar.hidden = false;
      bar.innerHTML = '<button type="button" class="tbtn active" data-m="raw">원본</button><button type="button" class="tbtn" data-m="json">JSON 정렬</button>';
      bar.addEventListener('click', (e) => {
        const b = e.target.closest('[data-m]');
        if (!b) return;
        $$('[data-m]', bar).forEach((x) => x.classList.toggle('active', x === b));
        render(b.dataset.m === 'json' ? pretty : text);
      });
    }
    const done = () => { wrap.remove(); document.removeEventListener('keydown', onKey, true); };
    const onKey = (e) => { if (e.key === 'Escape') { e.stopPropagation(); done(); } };
    wrap.addEventListener('click', (e) => {
      const b = e.target.closest('[data-v]');
      if (b?.dataset.v === 'copy') copyText(isNull ? '' : shown);
      if (b?.dataset.v === 'close') done();
    });
    document.addEventListener('keydown', onKey, true);
    document.body.appendChild(wrap);
    $('[data-v=close]', wrap).focus();
  }

  panels.addEventListener('contextmenu', (e) => {
    const td = e.target.closest('table.grid.resizable tbody td:not(.rn)');
    if (!td || td.classList.contains('editing')) return;
    e.preventDefault();
    const tr = td.closest('tr');
    const table = td.closest('table');
    $$('tr.sel', table).forEach((x) => x.classList.remove('sel'));
    tr.classList.add('sel');
    const info = cellInfo(td);
    const items = [
      { label: '값 복사', fn: () => copyText(info.text) },
      { label: '상세 보기…', fn: () => showCellViewer(td) },
      { label: '행 복사 (JSON)', fn: () => copyText(JSON.stringify(rowObject(tr), null, 2)) },
      { label: '행 복사 (탭 구분)', fn: () => copyText(Object.values(rowObject(tr)).map((v) => v ?? '').join('\t')) },
    ];
    if (table.classList.contains('editable') && td.dataset.col) {
      items.push('-', { label: '편집', fn: () => startEdit(td) }, {
        label: 'NULL로 설정',
        fn: () => {
          startEdit(td);
          const input = $('input', td);
          input.dataset.null = '1';
          input.value = '';
          commitEdit(td, input);
        },
      });
    }
    showMenu(e.clientX, e.clientY, items);
  });

  // ---- objects panel: view modes, name filter, info pane, ER diagram -------------
  function setObjMode(panel, mode) {
    panel.dataset.mode = mode;
    $$('.seg-btn', panel).forEach((b) => b.classList.toggle('active', b.dataset.mode === mode));
    $$('.obj-pane', panel).forEach((p) => { p.hidden = p.dataset.pane !== mode; });
    if (mode === 'er') loadErd(panel);
  }

  let erdScript;
  const loadErdScript = () => (window.DBCatERD ? Promise.resolve() : (erdScript ||= new Promise((resolve, reject) => {
    const s = document.createElement('script');
    s.src = '/static/erd.js';
    s.onload = resolve;
    s.onerror = () => { erdScript = null; reject(new Error('ER 모듈을 불러오지 못했습니다')); };
    document.head.appendChild(s);
  })));

  async function loadErd(panel) {
    if (panel._erd) { requestAnimationFrame(() => panel._erd.fit()); return; }
    const host = $('.er-host', panel);
    const { c, d, s } = ctxOf(panel);
    try {
      const [res] = await Promise.all([fetch('/erd/data?' + qs({ c, d, s })), loadErdScript()]);
      if (!res.ok) throw new Error((await res.text()).trim());
      const data = await res.json();
      if (!data.tables.length) { host.innerHTML = '<div class="empty">표시할 테이블이 없습니다.</div>'; return; }
      panel._erd = window.DBCatERD.render(host, data, {
        storageKey: [c, d, s].join('|'),
        onOpen: (t, isView) => openTable({ c, d, s, t }, isView),
        saveSVG: (text) => saveText(`erd-${s || d}.svg`, text, 'image/svg+xml'),
        savePNG: (b64) => saveBase64(`erd-${s || d}.png`, b64),
        onError: (m) => toast(m, 'error'),
      });
    } catch (err) {
      host.innerHTML = '';
      const box = document.createElement('div');
      box.className = 'errbar';
      box.textContent = String(err.message || err);
      host.appendChild(box);
    }
  }

  function loadInfo(panel) {
    if (!panel._sel) return;
    const { c, d, s } = ctxOf(panel);
    const tab = $('.info-tab.active', panel).dataset.info;
    const sel = panel._sel;
    const params = sel.kind && sel.kind !== 'table' ? { c, d, s, kind: sel.kind, id: sel.id, tab } : { c, d, s, t: sel.t, tab };
    htmx.ajax('GET', '/objects/info?' + qs(params), { target: $('.info-body', panel), swap: 'innerHTML' });
  }

  // The list body shown for a category (tables/views, indexes, triggers, routines); loaded on first use.
  function setObjCat(panel, cat) {
    panel.dataset.cat = cat;
    $$('.cat-tab', panel).forEach((b) => b.classList.toggle('active', b.dataset.cat === cat));
    panel._sel = null;
    $$('.sel', panel).forEach((x) => x.classList.remove('sel'));
    $('.info-body', panel).innerHTML = '<div class="empty">오브젝트를 선택하면 정보가 표시됩니다.</div>';
    const show = () => $$('.obj-body', panel).forEach((b) => { b.hidden = b.dataset.cat !== cat; });
    if ($(`.obj-body[data-cat="${cat}"]`, panel)) return show();
    const { c, d, s } = ctxOf(panel);
    const holder = document.createElement('div');
    $('.info-pane', panel).before(holder);
    htmx.ajax('GET', '/objects/cat?' + qs({ c, d, s, cat }), { target: holder, swap: 'outerHTML' }).then(show);
  }

  const OBJ_ROWS = '.orow, .orow-l, .otile, .orow-x';
  function objectsPanelClick(e, panel) {
    const cat = e.target.closest('.cat-tab');
    if (cat) return setObjCat(panel, cat.dataset.cat);
    const row = e.target.closest(OBJ_ROWS);
    if (row) {
      $$('.sel', panel).forEach((x) => { if (x.matches(OBJ_ROWS)) x.classList.remove('sel'); });
      if (row.matches('.orow-x')) {
        row.classList.add('sel');
        panel._sel = { kind: row.dataset.kind, id: row.dataset.id, name: row.dataset.name, t: row.dataset.tbl };
      } else {
        $$(`[data-t="${CSS.escape(row.dataset.t)}"]`, panel).forEach((x) => { if (x.matches(OBJ_ROWS)) x.classList.add('sel'); });
        panel._sel = { kind: 'table', t: row.dataset.t, view: row.dataset.view === '1' };
      }
      loadInfo(panel);
      return;
    }
    const seg = e.target.closest('.seg-btn');
    if (seg) return setObjMode(panel, seg.dataset.mode);
    const tab = e.target.closest('.info-tab');
    if (tab) {
      $$('.info-tab', panel).forEach((b) => b.classList.toggle('active', b === tab));
      return loadInfo(panel);
    }
    if (e.target.closest('.info-toggle')) {
      const ip = $('.info-pane', panel);
      ip.classList.toggle('collapsed');
    }
  }
  panels.addEventListener('input', (e) => {
    if (!e.target.matches('.obj-filter input')) return;
    const panel = e.target.closest('.objects-panel');
    const q = e.target.value.trim().toLowerCase();
    $$(OBJ_ROWS, panel).forEach((x) => {
      const name = (x.dataset.name || x.dataset.t || '').toLowerCase();
      x.hidden = !!q && !name.includes(q);
    });
    const er = $('.er-find input', panel);
    if (er && panel.dataset.mode === 'er') { er.value = q; er.dispatchEvent(new Event('input')); }
  });

  async function saveBase64(name, b64) {
    const app = window.go?.main?.App;
    if (app?.SaveBase64) {
      try {
        const p = await app.SaveBase64(name, b64);
        if (p) toast('저장했습니다: ' + p);
      } catch (err) { toast(String(err), 'error'); }
      return;
    }
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (ch) => ch.charCodeAt(0));
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([bytes], { type: 'image/png' }));
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 10000);
  }

  // ---- table data: form view -------------------------------------------------------
  // One row at a time as a vertical form. It reads the rows already rendered in the
  // grid (the current page), and edits go through the same /table/cell endpoint.
  async function postCell(form, tr, col, value, isNull) {
    const body = new URLSearchParams({
      c: form.dataset.c, d: form.dataset.d, s: form.dataset.s, t: form.dataset.t,
      pk: tr.dataset.pk, col, value: isNull ? '' : value, null: isNull ? '1' : '',
    });
    let res;
    try {
      res = await fetch('/table/cell', { method: 'POST', headers: { 'HX-Request': 'true' }, body });
    } catch { toast('요청을 보낼 수 없습니다', 'error'); return null; }
    if (!res.ok) {
      let msg = '저장하지 못했습니다';
      try { msg = JSON.parse(res.headers.get('HX-Trigger')).toast.msg; } catch { /* keep default */ }
      toast(msg, 'error');
      return null;
    }
    return res.text(); // the rendered cell
  }

  function toggleFormView(form) {
    const grid = $('.gridwrap', form);
    const existing = $('.formview', form);
    const btn = $('[data-act="form-view"]', form);
    if (existing) {
      existing.remove();
      grid.hidden = false;
      btn.innerHTML = `${svg('tb-form')}폼 보기`;
      return;
    }
    const heads = $$('thead th[data-col]', grid);
    const rows = $$('tbody tr[data-pk]', grid);
    if (!rows.length) return toast('표시할 행이 없습니다', 'error');
    const editable = $('table.grid', grid).classList.contains('editable');
    let idx = Math.max(0, rows.findIndex((r) => r.classList.contains('sel')));

    const view = document.createElement('div');
    view.className = 'formview';
    view.innerHTML = `<div class="fv-bar"><button type="button" class="tbtn" data-fv="prev" title="이전 행">${svg('prev')}</button>
      <span class="fv-pos"></span><button type="button" class="tbtn" data-fv="next" title="다음 행">${svg('next')}</button>
      <span class="tip">${editable ? '값을 바꾸고 Tab/클릭으로 벗어나면 저장됩니다 · Esc 되돌리기' : (form.dataset.t ? '읽기 전용' : '')}</span></div>
      <div class="fv-body"></div>`;
    grid.hidden = true;
    grid.after(view);
    btn.innerHTML = `${svg('tb-grid')}그리드 보기`;

    const pos = $('.fv-pos', view);
    const body = $('.fv-body', view);
    const prev = $('[data-fv=prev]', view);
    const next = $('[data-fv=next]', view);

    function show(i) {
      idx = Math.min(Math.max(i, 0), rows.length - 1);
      const tr = rows[idx];
      rows.forEach((r) => r.classList.toggle('sel', r === tr));
      pos.textContent = `${idx + 1} / ${rows.length} (현재 페이지)`;
      prev.disabled = idx === 0;
      next.disabled = idx === rows.length - 1;
      body.innerHTML = '';
      const tbl = document.createElement('table');
      tbl.className = 'fv-table';
      heads.forEach((th, i) => {
        const td = tr.querySelectorAll('td:not(.rn)')[i];
        const isNull = !!$('.null', td);
        const type = (th.title || '').split(' · ')[0];
        const row = document.createElement('tr');
        const label = document.createElement('th');
        label.innerHTML = `${th.querySelector('.pk') ? '<span class="pk">' + svg('key') + '</span>' : ''}<span class="fv-name"></span><small class="dim"></small>`;
        $('.fv-name', label).textContent = th.dataset.col;
        $('small', label).textContent = type;
        const cell = document.createElement('td');
        const ta = document.createElement('textarea');
        ta.className = 'fv-input';
        ta.rows = 1;
        ta.spellcheck = false;
        ta.value = isNull ? '' : td.textContent;
        ta.placeholder = isNull ? 'NULL' : '';
        ta.readOnly = !editable;
        const nullBox = document.createElement('label');
        nullBox.className = 'fv-null';
        nullBox.innerHTML = '<input type="checkbox"> NULL';
        const cb = $('input', nullBox);
        cb.checked = isNull;
        cb.disabled = !editable;
        ta.disabled = isNull && editable;
        const fit = () => { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight, 220) + 'px'; };
        let committed = { v: ta.value, n: isNull };
        const commit = async () => {
          const n = cb.checked;
          if (n === committed.n && (n || ta.value === committed.v)) return;
          const html = await postCell(form, tr, th.dataset.col, ta.value, n);
          if (html === null) { // refused: restore what the database still has
            ta.value = committed.v; cb.checked = committed.n; ta.disabled = committed.n && editable; return;
          }
          td.innerHTML = html;
          committed = { v: n ? '' : ta.value, n };
          ta.placeholder = n ? 'NULL' : '';
        };
        if (editable) {
          ta.addEventListener('blur', commit);
          ta.addEventListener('keydown', (ev) => {
            if (ev.key === 'Escape') { ta.value = committed.v; ev.preventDefault(); }
            if (ev.key === 'Enter' && !ev.shiftKey) { ev.preventDefault(); ta.blur(); }
          });
          ta.addEventListener('input', fit);
          cb.addEventListener('change', () => { ta.disabled = cb.checked; if (!cb.checked) ta.focus(); commit(); });
        }
        cell.append(ta, ...(editable ? [nullBox] : []));
        row.append(label, cell);
        tbl.appendChild(row);
        requestAnimationFrame(fit);
      });
      body.appendChild(tbl);
    }
    view.addEventListener('click', (e) => {
      const b = e.target.closest('[data-fv]');
      if (!b || b.disabled) return;
      document.activeElement?.blur?.(); // commit a pending edit first
      show(idx + (b.dataset.fv === 'next' ? 1 : -1));
    });
    show(idx);
  }

  // ---- SSH host key trust --------------------------------------------------
  document.addEventListener('click', async (e) => {
    const b = e.target.closest('[data-action="trust-host"]');
    if (!b) return;
    const { host, key, fp } = b.dataset;
    const msg = `SSH 서버 ${host} 의 호스트 키 지문입니다.\n\n${fp}\n\n`
      + '서버 관리자에게 받은 지문(또는 서버에서 ssh-keygen -lf 로 확인한 값)과 정확히 같을 때만 등록하세요. '
      + '등록하면 known_hosts 파일에 추가되어 이후 이 키로만 접속됩니다.';
    if (await confirmDialog(msg, '신뢰하고 등록', true)) {
      htmx.ajax('POST', '/conn/trust-host', { source: document.body, swap: 'none', values: { host, key, fp } });
    }
  });

  // ---- connection dialog ---------------------------------------------------
  const modal = $('#modal');
  let modalBack = null;
  const closeModal = () => {
    if (!modal.firstChild) return;
    modal.innerHTML = '';
    if (modalBack?.isConnected) modalBack.focus();
    modalBack = null;
  };
  document.body.addEventListener('htmx:beforeSwap', (e) => {
    if (e.target === modal && !modal.firstChild) modalBack = document.activeElement;
  });
  // Keep Tab inside the topmost dialog instead of wandering into the page behind it.
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Tab') return;
    const top = $$('.overlay').pop();
    if (!top) return;
    const items = $$('button, input, select, textarea, [tabindex="0"]', top)
      .filter((x) => !x.disabled && x.type !== 'hidden' && x.offsetParent !== null);
    if (!items.length) return;
    const first = items[0];
    const last = items[items.length - 1];
    if (!top.contains(document.activeElement)) { e.preventDefault(); first.focus(); }
    else if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });
  const closeModalNow = closeModal;
  // Defer: htmx fires the other HX-Trigger events on the form that lives inside
  // the modal; removing it first would make them miss document.body.
  document.body.addEventListener('close-modal', () => setTimeout(closeModal, 0));

  function syncKind(form) {
    const kind = $('#conn-kind', form).value;
    form.dataset.kind = kind;
    const sqlite = kind === 'sqlite';
    $$('.net input[name=database]', form).forEach((i) => { i.disabled = sqlite; });
    $$('.file input[name=database]', form).forEach((i) => { i.disabled = !sqlite; });
    $('#conn-port', form).placeholder = { postgres: '5432', mysql: '3306' }[kind] || '';
    form.dataset.ssh = $('#ssh-on', form).checked ? '1' : '';
  }

  modal.addEventListener('change', (e) => {
    if (e.target.id === 'conn-kind' || e.target.id === 'ssh-on') syncKind(e.target.form);
    if (e.target.classList.contains('ins-mode')) { // insert dialog: value / NULL / default
      const val = $('.ins-val', e.target.closest('.ins-row'));
      val.disabled = e.target.value !== 'v';
      if (!val.disabled) val.focus();
    }
  });
  modal.addEventListener('click', (e) => {
    const hist = e.target.closest('[data-act="hist-insert"]');
    if (hist) return insertHistory(hist.dataset.id);
    const b = e.target.closest('[data-action]');
    if (!b) return;
    if (b.dataset.action === 'close-modal') closeModal();
    if (b.dataset.action === 'pick-file') {
      const isKey = b.dataset.target === 'ssh_key';
      const input = $(isKey ? 'input[name=ssh_key]' : '.file input[name=database]', modal);
      window.go.main.App.PickFile(isKey ? 'SSH 개인키 선택' : 'SQLite 데이터베이스 파일 선택').then((p) => {
        if (p) input.value = p;
      }).catch((err) => toast(String(err), 'error'));
    }
  });
  document.body.addEventListener('htmx:afterSwap', (e) => {
    if (e.target !== modal) return;
    const ins = $('.ins-form', modal);
    if (ins) { $('.ins-val:not(:disabled)', ins)?.focus(); return; }
    const form = $('#connform', modal);
    if (!form) { if (!modal.contains(document.activeElement)) $('input:not([type=hidden]), select, button', modal)?.focus(); return; }
    syncKind(form);
    if (window.go?.main?.App?.PickFile) $$('[data-action=pick-file]', form).forEach((b) => { b.hidden = false; });
    $('input[name=name]', form).focus();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (!menu.hidden) hideMenu(true); else if (modal.firstChild) closeModal();
  });
})();
