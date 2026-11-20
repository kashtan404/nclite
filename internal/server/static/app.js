(() => {
'use strict';
const base = document.body.dataset.base;
const $ = id => document.getElementById(id);
if (!$('layout')) return; // the login page has no app

// ---------- small helpers (DOM is built with textContent only: file names are never parsed as HTML) ----------
const h = (tag, props, ...kids) => {
  const e = document.createElement(tag);
  for (const k in props || {}) {
    const v = props[k];
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (k === 'checked' || k === 'disabled' || k === 'controls') e[k] = !!v;
    else e.setAttribute(k, v);
  }
  for (const c of kids.flat()) if (c != null && c !== false) e.append(c.nodeType ? c : document.createTextNode(String(c)));
  return e;
};
const enc = encodeURIComponent;
const fmtSize = n => {
  if (n < 1024) return n + ' B';
  const u = ['KiB', 'MiB', 'GiB', 'TiB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(n < 10 ? 1 : 0) + ' ' + u[i];
};
const fmtDate = t => t ? new Date(t * 1000).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' }) : '';
let csrf = '';

let toastTimer;
function toast(msg, err) {
  const t = $('toast');
  t.textContent = msg;
  t.className = 'toast' + (err ? ' err' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add('hidden'), err ? 7000 : 2500);
}
const fail = e => toast(e && e.message ? e.message : String(e), true);

async function api(path, opt) {
  const r = await fetch(base + '/api/' + path, opt);
  let j = null;
  try { j = await r.json(); } catch (_) { /* not json */ }
  if (r.status === 401) { location.reload(); throw new Error('Session expired'); }
  if (!r.ok) throw new Error((j && j.error) || 'HTTP ' + r.status);
  return j;
}
const post = (path, body) => api(path, {
  method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf }, body: JSON.stringify(body),
});

const fileURL = (e, dl) => `${base}/file?path=${enc(e.path)}${dl ? '&dl=1' : ''}`;
const prevURL = (e, s) => `${base}/preview?id=${e.id}&s=${s}`;
const icon = e => e.dir ? '\u{1F4C1}' : !e.mime ? '\u{1F4C4}' : e.mime.startsWith('image/') ? '\u{1F5BC}' :
  e.mime.startsWith('video/') ? '\u{1F39E}' : e.mime.startsWith('audio/') ? '\u{1F3B5}' : e.mime === 'application/pdf' ? '\u{1F4D5}' : '\u{1F4C4}';
const ls = (k, d) => { try { return localStorage.getItem(k) ?? d; } catch (_) { return d; } };
const lset = (k, v) => { try { localStorage.setItem(k, v); } catch (_) { /* ignore */ } };

// ---------- state ----------
const st = {
  view: ls('view', 'small'), pane: ls('pane', '0') === '1', paneW: +ls('paneW', 440),
  sortKey: ls('sort', 'name'), sortDir: 1, limit: 400,
  where: { kind: 'dir', path: '' }, ro: false,
  entries: [], sel: new Set(), cur: null, albums: [],
};
const treeExpanded = new Set(['']);

// ---------- generic folder tree (sidebar and the folder picker) ----------
function makeTree(host, opts) {
  const exp = opts.expanded;
  async function build(path, name) {
    const arr = h('span', { class: 'arr' }, '\u25B8');
    const row = h('div', { class: 'tnode', 'data-path': path }, arr, h('span', { class: 'ico' }, '\u{1F4C1}'), h('span', { class: 'nm' }, name));
    const kids = h('ul');
    const li = h('li', null, row, kids);
    async function load() {
      kids.replaceChildren();
      const j = await api('tree?path=' + enc(path));
      const dirs = j.dirs || [];
      for (const d of dirs) kids.append(await build(d.path, d.name));
      if (!dirs.length) arr.textContent = '\u00B7';
    }
    async function expand() { exp.add(path); arr.textContent = '\u25BE'; await load(); }
    function collapse() { exp.delete(path); arr.textContent = '\u25B8'; kids.replaceChildren(); }
    arr.addEventListener('click', ev => { ev.stopPropagation(); if (exp.has(path)) collapse(); else expand().catch(fail); });
    row.addEventListener('click', () => { if (!exp.has(path)) expand().catch(fail); opts.onPick(path); });
    if (exp.has(path)) { arr.textContent = '\u25BE'; try { await load(); } catch (_) { exp.delete(path); } }
    return li;
  }
  return {
    async refresh() { host.replaceChildren(h('ul', null, await build('', 'My files'))); markTree(host, opts.selected()); },
  };
}
function markTree(host, path) {
  for (const n of host.querySelectorAll('.tnode')) n.classList.toggle('cur', n.dataset.path === path);
}
const sideTree = makeTree($('tree'), {
  expanded: treeExpanded,
  selected: () => (st.where.kind === 'dir' ? st.where.path : null),
  onPick: p => openDir(p).catch(fail),
});

// ---------- loading ----------
async function openDir(path) {
  const j = await api('list?path=' + enc(path));
  st.where = { kind: 'dir', path };
  st.entries = j.entries || []; st.ro = j.readonly; st.sel.clear(); st.cur = null; st.limit = 400;
  render();
}
async function openAlbum(id) {
  const j = await api('album?id=' + id);
  st.where = { kind: 'album', id, name: j.album.name };
  st.entries = j.entries || []; st.ro = false; st.sel.clear(); st.cur = null; st.limit = 400;
  render();
}
const reload = () => st.where.kind === 'dir' ? openDir(st.where.path) : openAlbum(st.where.id);
async function loadAlbums() { st.albums = (await api('albums')).albums; renderAlbums(); }
const afterChange = async () => { await reload(); await Promise.all([sideTree.refresh(), loadAlbums()]); };

// ---------- rendering ----------
function sorted() {
  const k = st.sortKey, d = st.sortDir;
  const val = e => k === 'size' ? e.size : k === 'date' ? e.mtime : e.name.toLowerCase();
  return st.entries.slice().sort((a, b) => {
    if (a.dir !== b.dir) return a.dir ? -1 : 1;
    const x = val(a), y = val(b);
    return (x < y ? -1 : x > y ? 1 : 0) * d;
  });
}
function renderCrumbs() {
  const c = $('crumbs'); c.replaceChildren();
  if (st.where.kind === 'album') { c.append(h('span', null, 'Albums / '), h('b', null, st.where.name)); return; }
  const parts = st.where.path ? st.where.path.split('/') : [];
  c.append(h('a', { onclick: () => openDir('').catch(fail) }, 'My files'));
  let acc = '';
  for (const p of parts) {
    acc = acc ? acc + '/' + p : p;
    const target = acc;
    c.append(h('span', null, ' / '), h('a', { onclick: () => openDir(target).catch(fail) }, p));
  }
}
function renderAlbums() {
  const box = $('albums'); box.replaceChildren();
  if (!st.albums.length) box.append(h('div', { class: 'mut' }, 'No albums yet'));
  for (const a of st.albums) {
    box.append(h('div', { class: 'alb' + (st.where.kind === 'album' && st.where.id === a.id ? ' cur' : ''),
      onclick: () => openAlbum(a.id).catch(fail) }, h('span', { class: 'nm ellipsis' }, a.name), h('span', { class: 'n' }, String(a.count))));
  }
}
function pick(e) {
  return h('input', { type: 'checkbox', checked: st.sel.has(e.path), onclick: ev => { ev.stopPropagation(); toggle(e, ev.target.checked); } });
}
function toggle(e, on) { if (on) st.sel.add(e.path); else st.sel.delete(e.path); updateToolbar(); }
const meta = e => e.dir ? '' : fmtSize(e.size);

function renderView() {
  const v = $('view'); v.replaceChildren();
  const all = sorted(), shown = all.slice(0, st.limit);
  if (!all.length) { v.append(h('div', { class: 'empty' }, st.where.kind === 'album' ? 'This album is empty. Select photos in a folder and use \u201CAdd to album\u201D.' : 'This folder is empty.')); return; }
  const open = e => ev => { ev.stopPropagation(); activate(e); };
  if (st.view === 'list') {
    for (const e of shown) {
      v.append(h('div', { class: 'row', 'data-p': e.path },
        pick(e), h('span', { class: 'ico' }, icon(e)),
        h('span', { class: 'nm ellipsis' }, h('a', { onclick: open(e) }, e.name), e.owner ? h('span', { class: 'mut' }, ' \u00B7 ' + e.owner) : null),
        h('span', { class: 'sz' }, meta(e)), h('span', { class: 'dt' }, fmtDate(e.mtime))));
    }
  } else if (st.view === 'table') {
    const th = (label, key, cls) => h('th', { class: cls || '', onclick: () => { if (st.sortKey === key) st.sortDir *= -1; else { st.sortKey = key; st.sortDir = 1; } $('sort').value = key; renderView(); } },
      label + (st.sortKey === key ? (st.sortDir > 0 ? ' \u25B2' : ' \u25BC') : ''));
    const t = h('table', { class: 'vt' }, h('tr', null, h('th'), th('Name', 'name'), h('th', null, 'Type'), th('Size', 'size', 'n'), th('Modified', 'date'), h('th', null, 'Owner')));
    for (const e of shown) {
      t.append(h('tr', { 'data-p': e.path }, h('td', { class: 'c' }, pick(e)),
        h('td', null, h('span', { class: 'ico' }, icon(e)), ' ', h('a', { onclick: open(e) }, e.name)),
        h('td', { class: 'mut' }, e.dir ? 'Folder' : (e.mime || '')), h('td', { class: 'n' }, meta(e)),
        h('td', { class: 'mut' }, fmtDate(e.mtime)), h('td', { class: 'mut' }, e.owner || '')));
    }
    v.append(t);
  } else {
    const size = st.view === 'large' ? 1024 : 512; // the preview that is requested; the browser scales it to the card
    const g = h('div', { class: 'grid ' + st.view });
    for (const e of shown) {
      const th = h('div', { class: 'th' });
      if (e.preview) th.append(h('img', { loading: 'lazy', alt: '', src: prevURL(e, size) }));
      else th.append(icon(e));
      g.append(h('div', { class: 'card', 'data-p': e.path, onclick: open(e) }, th,
        h('span', { class: 'pick' }, pick(e)), h('div', { class: 'cap', title: e.name }, e.name)));
    }
    v.append(g);
  }
  if (all.length > shown.length) {
    v.append(h('div', { class: 'empty' }, h('button', { onclick: () => { st.limit += 400; renderView(); } },
      `Show more (${all.length - shown.length} hidden)`)));
  }
  markCur();
}
function markCur() {
  for (const n of $('view').querySelectorAll('[data-p]')) n.classList.toggle('cur', !!st.cur && n.dataset.p === st.cur.path);
}
function render() {
  renderCrumbs(); renderView(); renderAlbums(); updateToolbar();
  markTree($('tree'), st.where.kind === 'dir' ? st.where.path : null);
  for (const b of $('views').children) b.classList.toggle('on', b.dataset.v === st.view);
  $('bPane').classList.toggle('on', st.pane);
}
function selected() { return st.entries.filter(e => st.sel.has(e.path)); }
function updateToolbar() {
  const s = selected(), n = s.length, album = st.where.kind === 'album';
  const anyRO = s.some(e => e.ro), files = s.filter(e => !e.dir).length;
  $('selInfo').textContent = n ? n + ' selected' : '';
  $('selAll').checked = n > 0 && n >= Math.min(st.entries.length, st.limit);
  $('bCopy').disabled = !n; $('bMove').disabled = !n || anyRO; $('bDelete').disabled = !n || anyRO;
  $('bRename').disabled = n !== 1 || anyRO;
  $('bAddAlbum').disabled = !files;
  $('bMkdir').classList.toggle('hidden', album); $('bMkdir').disabled = st.ro;
  $('bRemAlbum').classList.toggle('hidden', !album); $('bRemAlbum').disabled = !n;
  $('albumBtns').classList.toggle('hidden', !album);
}

// ---------- viewer pane ----------
function activate(e) {
  if (e.dir) { openDir(e.path).catch(fail); return; }
  st.cur = e; markCur();
  if (st.pane) showPane(e); else window.open(fileURL(e), '_blank', 'noopener');
}
function showPane(e) {
  $('pName').textContent = e.name; $('pName').title = e.name;
  $('pOpen').href = fileURL(e); $('pDl').href = fileURL(e, true);
  const b = $('pBody'); b.replaceChildren();
  const mt = e.mime || '';
  if (mt.startsWith('video/')) b.append(h('video', { controls: true, preload: 'metadata', src: fileURL(e) }));
  else if (mt.startsWith('audio/')) b.append(h('audio', { controls: true, src: fileURL(e) }));
  else if (mt === 'application/pdf') b.append(h('iframe', { src: fileURL(e), title: e.name }));
  else if (e.text) b.append(h('iframe', { src: fileURL(e), title: e.name, sandbox: '' }));
  else if (e.preview) b.append(h('img', { alt: e.name, src: prevURL(e, 1024) }));
  else if (/^image\/(jpeg|png|gif|webp|bmp)$/.test(mt)) b.append(h('img', { alt: e.name, src: fileURL(e) }));
  else b.append(h('div', { class: 'empty' }, 'No preview for this file type. Use Download.'));
  $('pInfo').textContent = [fmtSize(e.size), fmtDate(e.mtime), e.path, e.owner ? 'owner ' + e.owner : ''].filter(Boolean).join(' \u00B7 ');
}
function step(dir) {
  const files = sorted().filter(e => !e.dir);
  if (!files.length) return;
  let i = st.cur ? files.findIndex(e => e.path === st.cur.path) : -1;
  i = (i + dir + files.length) % files.length;
  st.cur = files[i]; markCur(); showPane(st.cur);
  const el = $('view').querySelector('[data-p].cur'); if (el && el.scrollIntoView) el.scrollIntoView({ block: 'nearest' });
}
function setPane(on) {
  st.pane = on; lset('pane', on ? '1' : '0');
  $('pane').classList.toggle('hidden', !on); $('split').classList.toggle('hidden', !on);
  $('pane').style.setProperty('--pw', st.paneW + 'px');
  $('bPane').classList.toggle('on', on);
  if (on) { if (st.cur) showPane(st.cur); else { $('pBody').replaceChildren(h('div', { class: 'empty' }, 'Click a file to view it here.')); $('pName').textContent = ''; $('pInfo').textContent = ''; } }
}
$('split').addEventListener('pointerdown', ev => {
  ev.preventDefault();
  const sp = $('split');
  if (sp.setPointerCapture) sp.setPointerCapture(ev.pointerId);
  const move = e => {
    st.paneW = Math.round(Math.min(Math.max(window.innerWidth - e.clientX, 240), window.innerWidth * 0.75));
    $('pane').style.setProperty('--pw', st.paneW + 'px');
  };
  const up = () => { sp.removeEventListener('pointermove', move); sp.removeEventListener('pointerup', up); lset('paneW', st.paneW); };
  sp.addEventListener('pointermove', move); sp.addEventListener('pointerup', up);
});

// ---------- dialogs ----------
function dialog(title, body, buttons) {
  return new Promise(resolve => {
    const m = $('modal');
    const close = v => { m.classList.add('hidden'); m.replaceChildren(); document.removeEventListener('keydown', onKey); resolve(v); };
    const onKey = ev => { if (ev.key === 'Escape') close(null); };
    document.addEventListener('keydown', onKey);
    const btns = h('div', { class: 'btns' }, buttons.map(b => h('button', { class: b.cls || '', onclick: () => close(b.value) }, b.label)));
    m.replaceChildren(h('div', { class: 'dlg' }, h('h3', null, title), body, btns));
    m.onclick = ev => { if (ev.target === m) close(null); };
    m.classList.remove('hidden');
  });
}
async function pickFolder(title) {
  let chosen = st.where.kind === 'dir' ? st.where.path : '';
  const host = h('div', { class: 'tree body' });
  const lbl = h('div', { class: 'mut' }, 'To: ' + (chosen || 'My files'));
  const t = makeTree(host, { expanded: new Set(['']), selected: () => chosen,
    onPick: p => { chosen = p; lbl.textContent = 'To: ' + (p || 'My files'); markTree(host, p); } });
  const p = dialog(title, h('div', null, host, lbl), [{ label: 'Cancel', value: null }, { label: 'Choose this folder', value: 'ok' }]);
  t.refresh().catch(fail);
  return (await p) === 'ok' ? chosen : null;
}
async function pickAlbum() {
  await loadAlbums();
  let chosen = null;
  const list = h('div', { class: 'body' });
  const mark = () => { for (const n of list.querySelectorAll('.alb')) n.classList.toggle('cur', Number(n.dataset.id) === chosen); };
  for (const a of st.albums) {
    list.append(h('div', { class: 'alb', 'data-id': String(a.id), onclick: () => { chosen = a.id; mark(); } },
      h('span', { class: 'nm ellipsis' }, a.name), h('span', { class: 'n' }, String(a.count))));
  }
  if (!st.albums.length) list.append(h('div', { class: 'mut' }, 'No albums yet. Create one with \u201CNew album\u2026\u201D.'));
  const r = await dialog('Add to album', list, [{ label: 'Cancel', value: null }, { label: 'New album\u2026', value: 'new' }, { label: 'Add', value: 'ok' }]);
  if (r === 'new') {
    const name = (window.prompt('Album name') || '').trim();
    if (!name) return null;
    return (await post('albumop', { op: 'create', name })).id;
  }
  return r === 'ok' ? chosen : null;
}

// ---------- actions ----------
async function fileOp(body) {
  const j = await post('op', body);
  const bad = j.results.filter(r => !r.ok);
  if (bad.length) toast(bad.slice(0, 3).map(r => r.path.split('/').pop() + ': ' + r.error).join('\n') + (bad.length > 3 ? ` (+${bad.length - 3} more)` : ''), true);
  else toast('Done');
  return j;
}
const paths = () => selected().map(e => e.path);

$('bCopy').onclick = () => run(async () => { const d = await pickFolder('Copy to'); if (d !== null) { await fileOp({ op: 'copy', items: paths(), dest: d }); await afterChange(); } });
$('bMove').onclick = () => run(async () => { const d = await pickFolder('Move to'); if (d !== null) { await fileOp({ op: 'move', items: paths(), dest: d }); await afterChange(); } });
$('bRename').onclick = () => run(async () => {
  const e = selected()[0];
  const name = window.prompt('New name', e.name);
  if (!name || name === e.name) return;
  await fileOp({ op: 'rename', items: [e.path], name: name.trim() }); await afterChange();
});
$('bDelete').onclick = () => run(async () => {
  const n = st.sel.size;
  if (!window.confirm(`Move ${n} item${n > 1 ? 's' : ''} to the trash?`)) return;
  await fileOp({ op: 'delete', items: paths() }); await afterChange();
});
$('bMkdir').onclick = () => run(async () => {
  const name = window.prompt('Folder name');
  if (!name) return;
  await fileOp({ op: 'mkdir', dest: st.where.path, name: name.trim() }); await afterChange();
});
$('bAddAlbum').onclick = () => run(async () => {
  const id = await pickAlbum();
  if (id == null) return;
  const j = await post('albumop', { op: 'add', id, items: selected().filter(e => !e.dir).map(e => e.path) });
  toast(`Added ${j.added} to the album` + (j.skipped ? `, ${j.skipped} skipped` : ''));
  await loadAlbums();
});
$('bRemAlbum').onclick = () => run(async () => {
  await post('albumop', { op: 'remove', id: st.where.id, ids: selected().map(e => e.id) });
  toast('Removed from the album (the files are untouched)'); await afterChange();
});
$('bAlbRename').onclick = () => run(async () => {
  const name = (window.prompt('Album name', st.where.name) || '').trim();
  if (!name || name === st.where.name) return;
  await post('albumop', { op: 'rename', id: st.where.id, name }); await afterChange();
});
$('bAlbDelete').onclick = () => run(async () => {
  if (!window.confirm(`Delete the album \u201C${st.where.name}\u201D? The photos themselves stay where they are.`)) return;
  await post('albumop', { op: 'delete', id: st.where.id });
  await loadAlbums(); await openDir('');
});
$('newAlbum').onclick = () => run(async () => {
  const name = (window.prompt('Album name') || '').trim();
  if (!name) return;
  const j = await post('albumop', { op: 'create', name });
  await loadAlbums(); await openAlbum(j.id);
});
async function run(fn) { try { await fn(); } catch (e) { fail(e); } }

$('selAll').onchange = ev => {
  const shown = sorted().slice(0, st.limit);
  if (ev.target.checked) for (const e of shown) st.sel.add(e.path); else st.sel.clear();
  renderView(); updateToolbar();
};
$('sort').value = st.sortKey;
$('sort').onchange = ev => { st.sortKey = ev.target.value; st.sortDir = 1; lset('sort', st.sortKey); renderView(); };
for (const b of $('views').children) b.onclick = () => { st.view = b.dataset.v; lset('view', st.view); render(); };
$('bPane').onclick = () => setPane(!st.pane);
$('pClose').onclick = () => setPane(false);
$('pPrev').onclick = () => step(-1);
$('pNext').onclick = () => step(1);
$('logout').onclick = () => run(async () => { await post('logout', {}); location.reload(); });
document.addEventListener('keydown', ev => {
  if (!st.pane || /INPUT|TEXTAREA|SELECT/.test(ev.target.tagName) || !$('modal').classList.contains('hidden')) return;
  if (ev.key === 'ArrowRight') step(1); else if (ev.key === 'ArrowLeft') step(-1);
});

// ---------- start ----------
(async () => {
  try {
    const me = await api('me');
    csrf = me.csrf; $('who').textContent = me.user;
    setPane(st.pane);
    await sideTree.refresh();
    await loadAlbums();
    await openDir('');
  } catch (e) { fail(e); }
})();
})();
