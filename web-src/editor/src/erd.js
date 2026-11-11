// ER diagram for dbcat. Layout: dagre (left-to-right, referenced tables on the
// left); look: rounded table cards joined by smooth curves. All colours
// are resolved from CSS variables at draw time so the SVG can be exported as is.
import dagre from '@dagrejs/dagre';

const HEAD = 24;
const ROW = 18;
const PAD = 8;
const NS = 'http://www.w3.org/2000/svg';
const el = (name, attrs = {}, parent) => {
  const e = document.createElementNS(NS, name);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  if (parent) parent.appendChild(e);
  return e;
};

let measureCtx;
const textWidth = (s) => {
  measureCtx ||= document.createElement('canvas').getContext('2d');
  measureCtx.font = '12px -apple-system, "Apple SD Gothic Neo", sans-serif';
  return measureCtx.measureText(s).width;
};

function palette(host) {
  const cs = getComputedStyle(host);
  const v = (n, d) => cs.getPropertyValue(n).trim() || d;
  return {
    head: v('--er-head', '#e0f0f2'), box: v('--er-box', '#ffffff'), line: v('--er-line', '#b9c6cc'),
    edge: v('--er-edge', '#5f7f8a'), text: v('--fg', '#172025'), dim: v('--fg-dim', '#62717b'),
    accent: v('--accent', '#0B7A85'), pk: '#d97706', hl: v('--tree-select', '#d8eef0'),
  };
}

function sizeOf(t) {
  let w = textWidth(t.name) + 2 * PAD + 14;
  for (const c of t.columns) w = Math.max(w, textWidth(c.name) + textWidth(c.type || '') + 2 * PAD + 44);
  return { w: Math.round(Math.min(Math.max(w, 150), 320)), h: HEAD + Math.max(t.columns.length, 1) * ROW + 6 };
}

/** Initial positions: dagre for connected tables, a grid below for the rest. */
function autoLayout(data, sizes) {
  const connected = new Set();
  for (const e of data.edges) { connected.add(e.table); connected.add(e.refTable); }
  const pos = {};
  let bottom = 20;
  let width = 0;
  if (connected.size) {
    const g = new dagre.graphlib.Graph({ multigraph: true });
    g.setGraph({ rankdir: 'LR', nodesep: 28, ranksep: 90, marginx: 20, marginy: 20 });
    g.setDefaultEdgeLabel(() => ({}));
    for (const name of connected) g.setNode(name, { width: sizes[name].w, height: sizes[name].h });
    data.edges.forEach((e, i) => g.setEdge(e.refTable, e.table, {}, String(i)));
    dagre.layout(g);
    for (const name of connected) {
      const n = g.node(name);
      pos[name] = { x: Math.round(n.x - n.width / 2), y: Math.round(n.y - n.height / 2) };
      bottom = Math.max(bottom, pos[name].y + n.height + 40);
      width = Math.max(width, pos[name].x + n.width);
    }
  }
  const lone = data.tables.filter((t) => !connected.has(t.name));
  const maxW = Math.max(width, 900);
  let x = 20;
  let y = bottom;
  let rowH = 0;
  for (const t of lone) {
    const s = sizes[t.name];
    if (x + s.w > maxW && x > 20) { x = 20; y += rowH + 24; rowH = 0; }
    pos[t.name] = { x, y };
    x += s.w + 28;
    rowH = Math.max(rowH, s.h);
  }
  return pos;
}

export function render(host, data, opts = {}) {
  host.innerHTML = '';
  host.classList.add('er');
  const storeKey = opts.storageKey ? `erd:${opts.storageKey}` : null;
  const sizes = {};
  for (const t of data.tables) sizes[t.name] = sizeOf(t);
  const byName = Object.fromEntries(data.tables.map((t) => [t.name, t]));
  const edges = data.edges.filter((e) => byName[e.table] && byName[e.refTable]);

  let saved = {};
  try { saved = storeKey ? JSON.parse(localStorage.getItem(storeKey) || '{}') : {}; } catch { saved = {}; }
  let pos = { ...autoLayout({ tables: data.tables, edges }, sizes), ...saved };
  for (const name of Object.keys(pos)) if (!byName[name]) delete pos[name];
  const persist = () => { try { if (storeKey) localStorage.setItem(storeKey, JSON.stringify(pos)); } catch { /* storage unavailable */ } };

  // ---- DOM: toolbar + canvas ------------------------------------------------
  const bar = document.createElement('div');
  bar.className = 'er-bar';
  bar.innerHTML = `<button type="button" data-er="fit" title="화면에 맞춤">맞춤</button>
    <button type="button" data-er="in" title="확대">＋</button><button type="button" data-er="out" title="축소">－</button>
    <button type="button" data-er="auto" title="자동 배치로 되돌리기">재배치</button>
    <span class="er-sep"></span>
    <button type="button" data-er="svg" title="SVG로 저장">SVG</button><button type="button" data-er="png" title="PNG로 저장">PNG</button>
    <label class="er-find"><input type="search" placeholder="테이블 찾기" spellcheck="false" autocomplete="off" aria-label="테이블 찾기"></label>
    <span class="er-info"></span>`;
  host.appendChild(bar);
  const info = bar.querySelector('.er-info');
  info.textContent = `테이블 ${data.tables.length}개 · 관계 ${edges.length}개${data.truncated ? ` (전체 ${data.total}개 중 일부만 표시)` : ''}`;
  const canvas = document.createElement('div');
  canvas.className = 'er-canvas';
  host.appendChild(canvas);
  const svg = el('svg', { class: 'er-svg', width: '100%', height: '100%' });
  canvas.appendChild(svg);
  const world = el('g', { class: 'er-world' }, svg);

  let view = { x: 0, y: 0, k: 1 };
  let selected = null;
  let filter = '';
  const applyView = () => world.setAttribute('transform', `translate(${view.x} ${view.y}) scale(${view.k})`);

  // ---- drawing --------------------------------------------------------------
  const anchorY = (name, cols) => {
    const t = byName[name];
    const i = Math.max(0, t.columns.findIndex((c) => c.name === cols[0]));
    return pos[name].y + HEAD + i * ROW + ROW / 2;
  };

  function edgePath(e) {
    const a = pos[e.table]; const b = pos[e.refTable];
    const sa = sizes[e.table]; const sb = sizes[e.refTable];
    const y1 = anchorY(e.table, e.columns);
    const y2 = anchorY(e.refTable, e.refColumns.length ? e.refColumns : [byName[e.refTable].columns.find((c) => c.pk)?.name]);
    if (e.table === e.refTable) { // self reference: loop on the right side
      const x = a.x + sa.w;
      return { d: `M${x} ${y1} C${x + 44} ${y1} ${x + 44} ${y2 + 10} ${x} ${y2 + 10}`, x1: x, y1, dir1: 1, x2: x, y2: y2 + 10, dir2: 1 };
    }
    let x1; let x2; let dir1; let dir2;
    if (b.x >= a.x + sa.w) { x1 = a.x + sa.w; x2 = b.x; dir1 = 1; dir2 = -1; }
    else if (b.x + sb.w <= a.x) { x1 = a.x; x2 = b.x + sb.w; dir1 = -1; dir2 = 1; }
    else { x1 = a.x + sa.w; x2 = b.x + sb.w; dir1 = 1; dir2 = 1; }
    if (dir1 === 1 && dir2 === 1) { // overlapping columns: go around the right side
      const x = Math.max(x1, x2) + 56;
      return { d: `M${x1} ${y1} C${x} ${y1} ${x} ${y2} ${x2} ${y2}`, x1, y1, dir1, x2, y2, dir2 };
    }
    const k = Math.max(40, Math.abs(x2 - x1) / 2);
    return { d: `M${x1} ${y1} C${x1 + dir1 * k} ${y1} ${x2 + dir2 * k} ${y2} ${x2} ${y2}`, x1, y1, dir1, x2, y2, dir2 };
  }

  const related = (name) => {
    const set = new Set([name]);
    for (const e of edges) { if (e.table === name) set.add(e.refTable); if (e.refTable === name) set.add(e.table); }
    return set;
  };
  const dimmed = (name) => {
    if (filter) return !name.toLowerCase().includes(filter);
    if (selected) return !related(selected).has(name);
    return false;
  };

  const edgeEls = [];
  const boxEls = {};

  function draw() {
    const c = palette(host);
    world.innerHTML = '';
    edgeEls.length = 0;
    const eg = el('g', { class: 'er-edges' }, world);
    const bg = el('g', { class: 'er-boxes' }, world);

    edges.forEach((e) => {
      const g = el('g', { class: 'er-edge', 'data-table': e.table, 'data-ref': e.refTable }, eg);
      const hit = el('path', { fill: 'none', stroke: 'transparent', 'stroke-width': 10 }, g);
      const path = el('path', { fill: 'none', stroke: c.edge, 'stroke-width': 1.3 }, g);
      const feet = el('path', { fill: 'none', stroke: c.edge, 'stroke-width': 1.3, 'stroke-linecap': 'round', 'stroke-linejoin': 'round' }, g);
      const dot = el('circle', { r: 3.2, fill: c.edge }, g);
      el('title', {}, g).textContent = `${e.table}(${e.columns.join(', ')}) → ${e.refTable}(${e.refColumns.join(', ')})${e.name ? ` · ${e.name}` : ''}${e.onDelete ? ` · ON DELETE ${e.onDelete}` : ''}`;
      edgeEls.push({ e, g, hit, path, feet, dot });
    });

    for (const t of data.tables) {
      const p = pos[t.name]; const s = sizes[t.name];
      const g = el('g', { class: 'er-table', 'data-table': t.name, transform: `translate(${p.x} ${p.y})`, style: 'cursor:grab' }, bg);
      el('rect', { width: s.w, height: s.h, rx: 8, fill: c.box, stroke: c.line, 'stroke-width': 1 }, g);
      el('path', { d: `M0 ${HEAD} V8 a8 8 0 0 1 8 -8 H${s.w - 8} a8 8 0 0 1 8 8 V${HEAD} z`, fill: t.view ? c.hl : c.head }, g);
      el('path', { d: `M0 ${HEAD}H${s.w}`, stroke: c.line, 'stroke-width': 1 }, g);
      const title = el('text', { x: PAD, y: 16, 'font-size': 12.5, 'font-weight': 600, fill: c.text }, g);
      title.textContent = t.name;
      if (t.view) {
        const tag = el('text', { x: s.w - PAD, y: 16, 'font-size': 9.5, 'font-weight': 600, 'text-anchor': 'end', fill: c.dim }, g);
        tag.textContent = 'VIEW';
      }
      t.columns.forEach((col, i) => {
        const y = HEAD + i * ROW + 13;
        if (col.pk) el('circle', { cx: PAD + 3, cy: y - 4, r: 3, fill: c.pk }, g); // primary key: solid dot
        else if (col.fk) el('circle', { cx: PAD + 3, cy: y - 4, r: 2.6, fill: 'none', stroke: c.edge, 'stroke-width': 1.3 }, g); // foreign key: ring
        const n = el('text', { x: PAD + 14, y, 'font-size': 11.5, 'font-weight': col.pk ? 700 : 400, fill: c.text }, g);
        n.textContent = col.name;
        const ty = el('text', { x: s.w - PAD, y, 'font-size': 10.5, 'text-anchor': 'end', fill: c.dim }, g);
        const label = col.type || '';
        ty.textContent = label.length > 18 ? label.slice(0, 17) + '…' : label;
      });
      boxEls[t.name] = g;
    }
    refresh();
  }

  function refresh() {
    const c = palette(host);
    for (const { e, g, hit, path, feet, dot } of edgeEls) {
      const r = edgePath(e);
      hit.setAttribute('d', r.d);
      path.setAttribute('d', r.d);
      // referencing (child) end: dot; referenced (parent) end: arrow head
      dot.setAttribute('cx', r.x1);
      dot.setAttribute('cy', r.y1);
      const b = r.dir2;
      feet.setAttribute('d', `M${r.x2 + b * 8} ${r.y2 - 4.5} L${r.x2} ${r.y2} L${r.x2 + b * 8} ${r.y2 + 4.5}`);
      const active = selected && (e.table === selected || e.refTable === selected);
      const faded = dimmed(e.table) || dimmed(e.refTable);
      path.setAttribute('stroke', active ? c.accent : c.edge);
      feet.setAttribute('stroke', active ? c.accent : c.edge);
      dot.setAttribute('fill', active ? c.accent : c.edge);
      path.setAttribute('stroke-width', active ? 2 : 1.3);
      g.setAttribute('opacity', faded && !active ? 0.18 : 1);
    }
    for (const [name, g] of Object.entries(boxEls)) {
      const rect = g.firstChild;
      rect.setAttribute('stroke', name === selected ? c.accent : c.line);
      rect.setAttribute('stroke-width', name === selected ? 2 : 1);
      g.setAttribute('opacity', dimmed(name) ? 0.28 : 1);
    }
  }

  // ---- view helpers ------------------------------------------------------------
  function bounds() {
    let x1 = Infinity; let y1 = Infinity; let x2 = -Infinity; let y2 = -Infinity;
    for (const t of data.tables) {
      const p = pos[t.name]; const s = sizes[t.name];
      x1 = Math.min(x1, p.x); y1 = Math.min(y1, p.y); x2 = Math.max(x2, p.x + s.w); y2 = Math.max(y2, p.y + s.h);
    }
    return Number.isFinite(x1) ? { x1: x1 - 24, y1: y1 - 24, x2: x2 + 40, y2: y2 + 24 } : { x1: 0, y1: 0, x2: 400, y2: 300 };
  }
  function fit() {
    const b = bounds(); const w = canvas.clientWidth || 800; const h = canvas.clientHeight || 500;
    const k = Math.min(1.2, Math.max(0.15, Math.min(w / (b.x2 - b.x1), h / (b.y2 - b.y1))));
    view = { k, x: (w - (b.x2 - b.x1) * k) / 2 - b.x1 * k, y: (h - (b.y2 - b.y1) * k) / 2 - b.y1 * k };
    applyView();
  }
  function zoomAt(cx, cy, factor) {
    const k = Math.min(3, Math.max(0.1, view.k * factor));
    const f = k / view.k;
    view = { k, x: cx - (cx - view.x) * f, y: cy - (cy - view.y) * f };
    applyView();
  }
  const toWorld = (clientX, clientY) => {
    const r = canvas.getBoundingClientRect();
    return { x: (clientX - r.left - view.x) / view.k, y: (clientY - r.top - view.y) / view.k };
  };

  // ---- interaction -------------------------------------------------------------
  canvas.addEventListener('wheel', (e) => {
    e.preventDefault();
    const r = canvas.getBoundingClientRect();
    zoomAt(e.clientX - r.left, e.clientY - r.top, e.deltaY < 0 ? 1.12 : 1 / 1.12);
  }, { passive: false });

  svg.addEventListener('pointerdown', (e) => {
    const tg = e.target.closest?.('.er-table');
    svg.setPointerCapture(e.pointerId);
    if (tg) {
      const name = tg.dataset.table;
      const start = toWorld(e.clientX, e.clientY);
      const orig = { ...pos[name] };
      let moved = false;
      tg.style.cursor = 'grabbing';
      const move = (ev) => {
        const w = toWorld(ev.clientX, ev.clientY);
        if (Math.abs(w.x - start.x) + Math.abs(w.y - start.y) > 3) moved = true;
        pos[name] = { x: Math.round(orig.x + w.x - start.x), y: Math.round(orig.y + w.y - start.y) };
        tg.setAttribute('transform', `translate(${pos[name].x} ${pos[name].y})`);
        refresh();
      };
      const up = () => {
        svg.removeEventListener('pointermove', move);
        svg.removeEventListener('pointerup', up);
        tg.style.cursor = 'grab';
        if (moved) persist();
        else { selected = selected === name ? null : name; refresh(); }
      };
      svg.addEventListener('pointermove', move);
      svg.addEventListener('pointerup', up);
      return;
    }
    const sx = e.clientX; const sy = e.clientY; const v0 = { ...view };
    let moved = false;
    canvas.classList.add('panning');
    const move = (ev) => {
      if (Math.abs(ev.clientX - sx) + Math.abs(ev.clientY - sy) > 3) moved = true;
      view = { ...v0, x: v0.x + ev.clientX - sx, y: v0.y + ev.clientY - sy };
      applyView();
    };
    const up = () => {
      svg.removeEventListener('pointermove', move);
      svg.removeEventListener('pointerup', up);
      canvas.classList.remove('panning');
      if (!moved && selected) { selected = null; refresh(); }
    };
    svg.addEventListener('pointermove', move);
    svg.addEventListener('pointerup', up);
  });
  svg.addEventListener('dblclick', (e) => {
    // Pointer capture retargets click events to the <svg>, so look under the cursor.
    const tg = document.elementFromPoint(e.clientX, e.clientY)?.closest?.('.er-table') || e.target.closest?.('.er-table');
    if (tg) opts.onOpen?.(tg.dataset.table, !!byName[tg.dataset.table]?.view);
  });

  bar.addEventListener('click', (e) => {
    const b = e.target.closest('[data-er]');
    if (!b) return;
    const k = b.dataset.er;
    if (k === 'fit') fit();
    else if (k === 'in') zoomAt(canvas.clientWidth / 2, canvas.clientHeight / 2, 1.25);
    else if (k === 'out') zoomAt(canvas.clientWidth / 2, canvas.clientHeight / 2, 0.8);
    else if (k === 'auto') {
      pos = autoLayout({ tables: data.tables, edges }, sizes);
      persist(); draw(); fit();
    } else if (k === 'svg') opts.saveSVG?.(exportSVG());
    else if (k === 'png') exportPNG().then((b64) => opts.savePNG?.(b64)).catch((err) => opts.onError?.(String(err)));
  });
  bar.querySelector('input').addEventListener('input', (e) => { filter = e.target.value.trim().toLowerCase(); refresh(); });

  // ---- export --------------------------------------------------------------------
  function exportSVG() {
    const b = bounds(); const w = Math.ceil(b.x2 - b.x1); const h = Math.ceil(b.y2 - b.y1);
    const c = palette(host);
    const clone = world.cloneNode(true);
    clone.setAttribute('transform', `translate(${-b.x1} ${-b.y1})`);
    clone.querySelectorAll('[style]').forEach((n) => n.removeAttribute('style'));
    const s = el('svg', { xmlns: NS, width: w, height: h, viewBox: `0 0 ${w} ${h}`, 'font-family': '-apple-system, "Apple SD Gothic Neo", sans-serif' });
    el('rect', { width: w, height: h, fill: getComputedStyle(canvas).backgroundColor || c.box }, s);
    s.appendChild(clone);
    return new XMLSerializer().serializeToString(s);
  }
  function exportPNG() {
    const text = exportSVG();
    const m = text.match(/width="(\d+)" height="(\d+)"/);
    const w = +m[1]; const h = +m[2];
    const scale = Math.min(2, 8000 / Math.max(w, h));
    return new Promise((resolve, reject) => {
      const img = new Image();
      img.onload = () => {
        const cv = document.createElement('canvas');
        cv.width = Math.round(w * scale); cv.height = Math.round(h * scale);
        const ctx = cv.getContext('2d');
        ctx.scale(scale, scale);
        ctx.drawImage(img, 0, 0);
        resolve(cv.toDataURL('image/png').split(',')[1]);
      };
      img.onerror = () => reject(new Error('PNG를 만들 수 없습니다'));
      img.src = 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(text);
    });
  }

  // Re-colour on theme change.
  const mo = new MutationObserver(() => draw());
  mo.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

  draw();
  requestAnimationFrame(fit);
  return {
    fit,
    focusTable(name) { selected = byName[name] ? name : null; refresh(); },
    exportSVG,
    destroy() { mo.disconnect(); host.innerHTML = ''; },
  };
}
