'use strict';

// The page holds no truth of its own. Everything it draws comes from the
// snapshot the server pushes on /api/events - the drives, what is in them,
// and every job - so two browsers looking at this server see the same thing
// without either of them asking. What the page does keep is what only it
// knows: which drive the user is looking at, which tab, which directory,
// and which files are ticked. Those survive every redraw, because losing a
// selection of forty files to a progress update would be maddening.

const el = (id) => document.getElementById(id);

const state = {
  status: null,      // /api/status, read once
  drives: [],        // from the snapshot
  jobs: [],
  selected: null,    // drive id
  tab: 'rip',
  detail: null,      // /api/drives/<id>, which includes the volume and capabilities
  discKey: '',       // identifies the disc; a change means reload everything below
  path: '/',
  entries: [],
  picked: new Set(),
  images: [],
  isos: [],
  // What the selected burn image turned out to be, from /api/imageinfo.
  burnInfo: null,
  formats: [],
  discs: [],
  // What the archive dialog is for when it opens: a download of one folder,
  // or a rip of whatever is ticked.
  archiveFor: null,
  // Which row the image picker is on, for the keyboard.
  pickAt: 0,
  // Whether the image list is showing everything, or only its first page.
  imagesAll: false,
};

/* ---------- small helpers ---------- */

function bytes(n) {
  if (n === null || n === undefined) return '-';
  if (n < 1024) return `${n} B`;
  const units = ['kB', 'MB', 'GB', 'TB'];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

function rate(n) { return n > 0 ? `${bytes(n)}/s` : ''; }

function duration(sec) {
  if (!sec && sec !== 0) return '-';
  const s = Math.round(sec);
  const m = Math.floor(s / 60);
  return `${m}:${String(s % 60).padStart(2, '0')}`;
}

// eta is rounded to what is worth saying: nobody needs "4 minutes and 37
// seconds left" on a job that will not keep to it anyway.
function eta(seconds) {
  if (!seconds || seconds <= 0) return '';
  if (seconds < 45) return 'under a minute left';
  const mins = Math.round(seconds / 60);
  if (mins < 60) return `about ${mins} minute${mins > 1 ? 's' : ''} left`;
  const hours = Math.floor(mins / 60);
  const rest = mins % 60;
  return `about ${hours}h ${rest}m left`;
}

function when(iso) {
  if (!iso || iso.startsWith('0001')) return '-';
  const d = new Date(iso);
  if (isNaN(d)) return '-';
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
}

function since(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return '';
  return duration((Date.now() - d.getTime()) / 1000);
}

// text() rather than innerHTML anywhere a disc, a file or a drive has had a
// say in the content: a volume label is whatever was mastered onto the disc,
// and it is not this page's business to execute it.
function text(tag, s, cls) {
  const n = document.createElement(tag);
  n.textContent = s === null || s === undefined ? '' : String(s);
  if (cls) n.className = cls;
  return n;
}

/* ---------- keeping the page still ---------- */

// The server pushes a whole snapshot of itself twice a second, and this page
// used to rebuild its DOM from each one. That is what made it flicker: the
// row under the cursor was destroyed and built again between the press of a
// mouse button and its release, a menu could not be held open, and every
// list jumped as it was replaced. None of it was a paint problem - it was
// the page throwing away the thing you were pointing at.
//
// So nothing below rebuilds. sync() keeps a list of rows in step with a list
// of items by key, creating only what is new and leaving everything else
// exactly where it is; memo() skips work whose inputs have not changed; and
// the setters below write to the DOM only when the value is different,
// because assigning the same string still costs a style recalculation and,
// on a text node inside a selection, the selection.

function sync(container, items, keyOf, create, update) {
  const have = new Map();
  for (const node of Array.from(container.children)) {
    const k = node.dataset.key;
    if (k === undefined) { node.remove(); continue; }
    have.set(k, node);
  }
  let prev = null;
  for (const item of items) {
    const k = String(keyOf(item));
    let node = have.get(k);
    if (node) {
      have.delete(k);
    } else {
      node = create(item);
      node.dataset.key = k;
    }
    update(node, item);
    // Move it only if it is not already in the right place: moving a node
    // that holds the focus takes the focus with it.
    const want = prev ? prev.nextSibling : container.firstChild;
    if (node !== want) container.insertBefore(node, want);
    prev = node;
  }
  for (const node of have.values()) node.remove();
}

const memos = new Map();

// memo runs fn only when its inputs have actually changed. Most of what
// arrives in a snapshot is identical to the last one - a disc does not
// change twice a second - so this is what turns a redraw of everything into
// a redraw of the one figure that moved.
function memo(name, inputs, fn) {
  const key = JSON.stringify(inputs);
  if (memos.get(name) === key) return false;
  memos.set(name, key);
  fn();
  return true;
}

// forget makes the next render of name unconditional. Used where something
// outside the snapshot changed, such as a disc being swapped.
function forget(name) {
  for (const k of Array.from(memos.keys())) {
    if (k === name || k.startsWith(name + ':')) memos.delete(k);
  }
}

function setText(node, s) {
  const v = s === null || s === undefined ? '' : String(s);
  if (node.textContent !== v) node.textContent = v;
}

function setHidden(node, hidden) {
  if (node.hidden !== !!hidden) node.hidden = !!hidden;
}

function setDisabled(node, off) {
  if (node.disabled !== !!off) node.disabled = !!off;
}

function setClass(node, cls, on) {
  if (node.classList.contains(cls) !== !!on) node.classList.toggle(cls, !!on);
}

function setWidth(node, w) {
  if (node.style.width !== w) node.style.width = w;
}

/* ---------- errors ---------- */

function fail(msg) {
  el('error').textContent = msg;
  el('error').hidden = !msg;
}

async function api(path, opts) {
  const r = await fetch(path, opts);
  if (r.status === 401) {
    window.location.replace('/login');
    throw new Error('signed out');
  }
  if (!r.ok) {
    let msg = `HTTP ${r.status}`;
    try { msg = (await r.json()).error || msg; } catch (e) { /* not JSON */ }
    throw new Error(msg);
  }
  if (r.status === 204) return null;
  return r.json();
}

const post = (path, body) => api(path, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: body === undefined ? undefined : JSON.stringify(body),
});

/* ---------- drives ---------- */

function driveByID(id) { return state.drives.find((d) => d.id === id) || null; }

// forgetDisc drops what was memoised about whatever was in the drive, so a
// swapped disc is drawn again rather than compared against the last one.
function forgetDisc() {
  for (const name of ['dischead', 'rip', 'scan', 'burn', 'append', 'files', 'audio', 'drive']) {
    forget(name);
  }
}

// discKey changes exactly when the disc does. It is what decides whether the
// file listing and the volume details still describe what is in the drive.
function discKey(d) {
  if (!d || !d.disc || !d.disc.present) return 'empty';
  return [d.disc.profile, d.disc.sectors, d.disc.tracks ? d.disc.tracks.length : 0].join('/');
}

function renderDrives() {
  setHidden(el('noDrives'), state.drives.length > 0);
  sync(el('drives'), state.drives, (d) => d.id, createDriveRow, updateDriveRow);
  if (!state.selected && state.drives.length === 1) select(state.drives[0].id);
  if (state.selected && !driveByID(state.selected)) select(null);
}

// A drive is a row built once. Everything that changes about it - what is
// in it, whether a job has it - is written into the same nodes afterwards.
function createDriveRow(d) {
  const row = document.createElement('div');
  row.className = 'drive';

  const pick = document.createElement('button');
  pick.type = 'button';
  pick.className = 'pick';
  pick.appendChild(text('div', '', 'dname'));
  pick.appendChild(text('div', '', 'dwhat'));
  pick.addEventListener('click', () => select(d.id));
  row.appendChild(pick);

  row.appendChild(text('span', '', 'chip'));

  if (state.status && state.status.allowEject) {
    const tray = document.createElement('div');
    tray.className = 'tray';
    tray.appendChild(trayButton(d, 'eject', 'Open'));
    tray.appendChild(trayButton(d, 'load', 'Close'));

    const refresh = text('button', 'Re-read');
    refresh.type = 'button';
    refresh.dataset.act = 'refresh';
    refresh.title = 'Forget what is known about the disc and ask the drive again';
    refresh.addEventListener('click', async () => {
      refresh.disabled = true;
      try { await post(`/api/drives/${encodeURIComponent(d.id)}/refresh`); fail(''); }
      catch (e) { fail(String(e.message || e)); }
      if (d.id === state.selected) loadDetail();
    });
    tray.appendChild(refresh);
    row.appendChild(tray);
  }
  return row;
}

function updateDriveRow(row, d) {
  setClass(row, 'on', d.id === state.selected);
  row.querySelector('.pick').setAttribute('aria-pressed', d.id === state.selected ? 'true' : 'false');
  setText(row.querySelector('.dname'), d.name || d.id);
  setText(row.querySelector('.dwhat'), `${d.path} \u00b7 ${describeDisc(d)}`);

  const chip = row.querySelector('.chip');
  if (d.busy) { setText(chip, 'working'); chip.className = 'chip busy'; }
  else if (!d.disc || !d.disc.present) { setText(chip, 'empty'); chip.className = 'chip none'; }
  else { setText(chip, d.disc.profileName); chip.className = 'chip'; }

  for (const b of row.querySelectorAll('.tray button')) setDisabled(b, !!d.busy);
}

// trayButton opens or closes one drive. It is disabled while a job has the
// drive, because a tray opening mid-rip is how a disc gets scratched.
function trayButton(d, action, label) {
  const b = text('button', label);
  b.type = 'button';
  b.dataset.act = action;
  b.title = action === 'eject'
    ? 'Open this drive\u2019s tray'
    : 'Close this drive\u2019s tray and read what is in it';
  b.addEventListener('click', async () => {
    b.disabled = true;
    try {
      await post(`/api/drives/${encodeURIComponent(d.id)}/tray/${action}`);
      fail('');
    } catch (e) {
      fail(String(e.message || e));
      b.disabled = false;
    }
  });
  return b;
}

function describeDisc(d) {
  if (d.error) return d.error;
  if (!d.disc || !d.disc.present) return d.disc && d.disc.error ? d.disc.error : 'no disc';
  const bits = [];
  if (d.disc.statusName) bits.push(d.disc.statusName);
  if (d.disc.sectors) bits.push(bytes(d.disc.dataBytes));
  if (d.disc.audioTracks) bits.push(`${d.disc.audioTracks} audio track${d.disc.audioTracks > 1 ? 's' : ''}`);
  if (d.disc.dataTracks) bits.push('data');
  return bits.join(' · ');
}

async function select(id) {
  state.selected = id;
  state.detail = null;
  state.discKey = '';
  state.path = '/';
  state.picked.clear();
  // Everything remembered below was remembered about another drive.
  forgetDisc();
  renderDrives();
  el('work').hidden = !id;
  if (!id) return;
  await loadDetail();
}

// loadDetail fetches the deep view of the selected drive: the volume
// descriptors and the capabilities, which the list deliberately does not
// read because each costs a seek.
async function loadDetail() {
  const id = state.selected;
  if (!id) return;
  try {
    state.detail = await api(`/api/drives/${encodeURIComponent(id)}`);
    state.discKey = discKey(state.detail);
    fail('');
  } catch (e) {
    state.detail = null;
    fail(String(e.message || e));
  }
  renderWork();
  if (state.tab === 'files') loadDir(state.path);
}

/* ---------- the workspace ---------- */

// The panes, in the order they appear. Which of them are offered depends on
// what is in the drive: a disc with no filesystem has nothing to browse, and
// a machine that cannot burn is not asked about burning. Hiding what cannot
// be done is most of what makes this page quiet.
const panes = {
  rip:   { id: 'paneRip',   label: 'Rip' },
  files: { id: 'paneFiles', label: 'Files' },
  audio: { id: 'paneAudio', label: 'Audio' },
  check: { id: 'paneCheck', label: 'Check' },
  burn:  { id: 'paneBurn',  label: 'Burn' },
  add:   { id: 'paneAdd',   label: 'Add files' },
  drive: { id: 'paneDrive', label: 'Drive' },
};

function segItems(d) {
  const present = !!(d.disc && d.disc.present);
  const burning = !!(state.status && state.status.allowBurn);
  const out = [];
  // An empty drive can be asked about itself, and - if it could take a
  // blank - about burning. Nothing else applies.
  if (present) {
    if (d.canRipIso || d.canRipImg || d.canRipAudio) out.push('rip');
    if (d.canBrowse) out.push('files');
    if (d.disc.audioTracks > 0) out.push('audio');
    out.push('check');
  }
  // Burning is offered whenever this server can burn: the pane says why a
  // particular disc cannot be, which is what somebody holding a blank needs
  // to read. Adding to a disc is offered only when this disc actually takes
  // it, because there is nothing to do about a disc that does not.
  if (burning) out.push('burn');
  if (burning && present && d.canAppend) out.push('add');
  out.push('drive');
  return out;
}

// showPane is the cheap half: which pane is visible and which button looks
// pressed. It runs on every snapshot, so it must not start anything.
function showPane(tab) {
  state.tab = tab;
  for (const [name, pane] of Object.entries(panes)) {
    setHidden(el(pane.id), name !== tab);
  }
  for (const b of el('seg').children) {
    const on = b.dataset.key === tab;
    setClass(b, 'active', on);
    b.setAttribute('aria-pressed', on ? 'true' : 'false');
  }
}

// setTab is what a press does, and the only thing that may fetch. Reading a
// directory from here rather than from showPane matters: showPane runs twice
// a second, and a disc whose root is genuinely empty would have had its
// listing requested twice a second for as long as the tab was open.
function setTab(tab) {
  showPane(tab);
  if (state.detail) renderPane(state.detail);
  if (tab === 'files' && state.entries.length === 0) loadDir(state.path);
}

function renderSeg(d) {
  const items = segItems(d);
  sync(el('seg'), items, (k) => k, (k) => {
    const b = text('button', panes[k].label);
    b.type = 'button';
    b.addEventListener('click', () => setTab(k));
    return b;
  }, () => {});
  // The pane that was open may no longer be on offer - a disc came out, or
  // one with no filesystem went in. Falling back to the first thing that is
  // counts as a press, because it is a pane nobody has opened yet.
  if (!items.includes(state.tab)) setTab(items[0]);
  else showPane(state.tab);
  setHidden(el('seg'), items.length === 0);
}

function renderWork() {
  const d = state.detail;
  if (!d) return;
  setText(el('workTitle'), `${d.name || d.id} — ${d.path}`);
  renderDiscHead(d);
  renderSeg(d);
  renderPane(d);
}

// Only the pane that is on screen is drawn. A pane that is hidden is drawn
// when it is opened, and the memo below it keeps that from costing anything
// when nothing has changed since.
function renderPane(d) {
  switch (state.tab) {
    case 'rip': renderRipPane(d); break;
    case 'files': renderFilesTab(d); break;
    case 'audio': renderAudioTab(d); break;
    case 'check': renderScanPanel(d); break;
    case 'burn': renderBurnBox(d); break;
    case 'add': renderAppendBox(d); break;
    case 'drive': renderDriveTab(d); break;
  }
}

function dl(target, pairs) {
  target.textContent = '';
  for (const [k, v] of pairs) {
    if (v === null || v === undefined || v === '') continue;
    target.appendChild(text('dt', k));
    target.appendChild(text('dd', v));
  }
}

/* ---------- the disc itself ---------- */

// The heading is what is in the drive, in as few words as it takes. The
// thirteen-row fact list this page used to show at all times is behind the
// disclosure under it: one of those rows answers a question someone had,
// and twelve of them are furniture.
function renderDiscHead(d) {
  const disc = d.disc;
  const vol = d.volume;
  const present = !!(disc && disc.present);

  memo('dischead', [present, disc && disc.profileName, disc && disc.statusName,
    disc && disc.sectors, disc && disc.dataBytes, disc && disc.audioTracks,
    disc && disc.sessions, vol && vol.volumeId, vol && vol.format,
    d.error, disc && disc.error], () => {
    setText(el('discTitle'), present
      ? ((vol && vol.volumeId) || (disc.profileName ? `a ${disc.profileName}` : 'a disc'))
      : 'Nothing in the drive');
    // An empty drive says so once, in the heading. The line under it is for
    // what to do about it - or for a fault, which is the one case where the
    // drive has something to say that the heading does not.
    setText(el('discLine'), present ? discSummary(d)
      : (d.error || 'Close the tray with the button on the drive above, or press Re-read.'));

    const tags = [];
    if (present) {
      if (disc.profileName) tags.push(['chip', disc.profileName]);
      if (vol && vol.format) tags.push(['chip', vol.format]);
      if (disc.statusName) tags.push(['chip none', disc.statusName]);
    }
    sync(el('discTags'), tags, (t) => t[1],
      () => text('span', '', 'chip'),
      (node, t) => { setText(node, t[1]); node.className = t[0]; });

    setHidden(el('discMore'), !present);
    if (present) renderDiscFacts(d);
  });
}

function discSummary(d) {
  const disc = d.disc;
  const bits = [];
  if (disc.dataTracks && disc.dataBytes) bits.push(`${bytes(disc.dataBytes)} of data`);
  if (disc.audioTracks) bits.push(`${disc.audioTracks} audio track${disc.audioTracks > 1 ? 's' : ''}`);
  if (disc.sessions > 1) bits.push(`${disc.sessions} sessions`);
  if (d.volume && d.volume.format) bits.push(`read as ${d.volume.format}`);
  if (!bits.length) bits.push(disc.statusName || 'nothing readable on it');
  return bits.join(' · ');
}

function renderDiscFacts(d) {
  const disc = d.disc;
  const vol = d.volume;
  dl(el('discFacts'), [
    ['Disc', disc.profileName],
    ['State', disc.statusName + (disc.erasable ? ', erasable' : '')],
    ['Sessions', disc.sessions || null],
    ['Tracks', `${disc.tracks ? disc.tracks.length : 0}` +
      (disc.audioTracks ? ` (${disc.audioTracks} audio)` : '')],
    ['Sectors', disc.sectors ? disc.sectors.toLocaleString() : null],
    ['As .iso', disc.dataTracks ? bytes(disc.dataBytes) : null],
    ['As .img', disc.rawReadable ? bytes(disc.rawBytes) : null],
    ['Volume', vol ? vol.volumeId : null],
    ['Filesystem', vol ? (vol.format || 'ISO 9660') : null],
    ['Published', vol && vol.publisher ? vol.publisher : null],
    ['Mastered', vol && vol.created ? when(vol.created) : null],
    ['Names', vol ? namingScheme(vol) : null],
    ['Media id', disc.mediaId || null],
  ]);
}

function renderRipPane(d) {
  memo('rip', [d.canRipIso, d.canRipImg, d.canRipAudio, d.busy,
    d.browseError, d.disc && d.disc.present, d.disc && d.disc.profile], () => {
    // The menu offers only what this disc and this drive can actually do,
    // so nothing here fails after it is pressed.
    const kinds = [];
    if (d.canRipIso) kinds.push(['iso', '.iso - the filesystem, 2048 bytes a sector']);
    if (d.canRipImg) kinds.push(['img', '.img - every byte on the disc, with a cue sheet']);
    if (d.canRipAudio) kinds.push(['audio', '.wav - one file per audio track']);
    const sel = el('ripKind');
    const was = sel.value;
    sel.textContent = '';
    for (const [v, label] of kinds) {
      const o = document.createElement('option');
      o.value = v; o.textContent = label;
      sel.appendChild(o);
    }
    if (kinds.some((k) => k[0] === was)) sel.value = was;
    setDisabled(el('btnRip'), kinds.length === 0 || !!d.busy);
    setHidden(el('ripLengthField'), sel.value !== 'iso');
    setText(el('ripHint'), ripHint(d));
  });
}

function ripHint(d) {
  if (!d.canRipIso && !d.canRipImg && !d.canRipAudio) {
    return d.browseError || 'There is nothing on this disc that this drive can read.';
  }
  const bits = [];
  if (!d.canRipImg && d.disc && d.disc.profile && d.disc.present) {
    bits.push('A raw .img is not offered: this drive will not hand over 2352-byte sectors from this disc.');
  }
  bits.push('Sectors the drive cannot read are written as zeroes and listed on the job, so a damaged disc still yields everything else.');
  return bits.join(' ');
}

function burnSource() {
  return el('burnSource').value === 'isos' ? state.isos : state.images;
}

/* ---------- the image picker ---------- */

// A file the drive could not fit on anything it writes. The server names
// the smallest disc an image needs; this is what it says when there is not
// one.
const noDiscFits = 'nothing this drive writes';

// What is worth knowing about an image at the moment of choosing it. It all
// comes from the listing, which already looked inside each file: an
// installer is chosen by what it boots, and that is not in its name.
function imageTags(f) {
  const tags = [];
  if (isArchiveName(f.name)) {
    // What will happen to it, which is the thing worth knowing before
    // spending a disc: not the archive, the files in it.
    tags.push(['pack', 'unpacked to files']);
    if (f.needs) tags.push(['disc', `needs ${f.needs}`]);
    return tags;
  }
  const boot = f.boot;
  if (boot && boot.bootable) {
    for (const p of boot.platforms || []) tags.push(['fw', p.name]);
    for (const a of boot.architectures || []) tags.push(['arch', a]);
  } else if (f.error) {
    tags.push(['bad', 'not an ISO']);
  } else if (boot) {
    tags.push(['bad', 'will not boot']);
  }
  if (f.needs === noDiscFits) tags.push(['bad', 'too big for any disc']);
  else if (f.needs) tags.push(['disc', `needs ${f.needs}`]);
  return tags;
}

function tagEl(cls, text) {
  const s = document.createElement('span');
  s.className = `tag-x ${cls}`;
  s.textContent = text;
  return s;
}

// The archives ripperX can read back. A disc ripped into one is written
// back out by unpacking it, so these belong in the burn menu beside the
// images even though they are not images at all.
const archiveSuffixes = ['.zip', '.tar', '.tar.gz', '.tgz', '.tar.xz', '.txz',
  '.tar.bz2', '.tbz2', '.tbz'];

function isArchiveName(name) {
  const lower = String(name).toLowerCase();
  return archiveSuffixes.some((x) => lower.endsWith(x));
}

// Everything the picker can be asked for, in the order it is listed: an
// image that is a whole number of sectors, or an archive whose contents can
// be written as a filesystem.
function pickable() {
  return burnSource().filter((f) =>
    (f.size > 0 && f.size % 2048 === 0) || isArchiveName(f.name));
}

// Typing filters on everything shown, not only the name: "uefi", "aarch64"
// and "dvd" are the three things somebody standing at a drive is actually
// looking for.
function pickMatches() {
  const q = el('burnFilter').value.trim().toLowerCase();
  const all = pickable();
  if (!q) return all;
  return all.filter((f) => {
    const hay = [f.name, f.volume || '', f.needs || '']
      .concat(imageTags(f).map(([, t]) => t)).join(' ').toLowerCase();
    return q.split(/\s+/).every((word) => hay.includes(word));
  });
}

function currentImage() {
  const name = el('burnImage').value;
  return pickable().find((f) => f.name === name) || null;
}

// The button says what is chosen, with the same tags the menu shows, so
// nothing has to be reopened to check what is about to be written.
function renderPickButton() {
  const btn = el('burnPick');
  btn.textContent = '';
  const f = currentImage();
  const name = document.createElement('span');
  name.className = 'pick-name';
  if (!f) {
    name.classList.add('pick-none');
    name.textContent = pickable().length ? 'Choose an image' : 'Nothing to burn';
    btn.appendChild(name);
    return;
  }
  name.textContent = f.name;
  btn.appendChild(name);
  for (const [cls, text] of imageTags(f).slice(0, 3)) btn.appendChild(tagEl(cls, text));
}

function renderPickList() {
  const list = el('burnList');
  const files = pickMatches();
  list.textContent = '';
  const chosen = el('burnImage').value;
  files.forEach((f, i) => {
    const opt = document.createElement('button');
    opt.type = 'button';
    opt.className = 'pick-opt';
    opt.setAttribute('role', 'option');
    opt.setAttribute('aria-selected', String(f.name === chosen));
    opt.id = `pickOpt${i}`;
    if (i === state.pickAt) opt.classList.add('here');

    const top = document.createElement('div');
    top.className = 'pick-top';
    const file = document.createElement('span');
    file.className = 'pick-file';
    file.textContent = f.name;
    const size = document.createElement('span');
    size.className = 'pick-size';
    size.textContent = bytes(f.size);
    top.append(file, size);
    opt.appendChild(top);

    const tags = imageTags(f);
    if (f.volume) tags.unshift(['', f.volume]);
    if (tags.length) {
      const row = document.createElement('div');
      row.className = 'pick-tags';
      for (const [cls, text] of tags) row.appendChild(tagEl(cls, text));
      opt.appendChild(row);
    }
    opt.addEventListener('click', () => { chooseImage(f.name); closePicker(); });
    list.appendChild(opt);
  });
  const empty = el('burnEmpty');
  empty.hidden = files.length > 0;
  if (!files.length) {
    empty.textContent = pickable().length
      ? 'Nothing here matches that.'
      : 'Nothing in this library is a whole number of 2048-byte sectors.';
  }
  const here = list.querySelector('.here');
  if (here) {
    // Guarded because this is the one call on the page that a DOM is
    // allowed not to implement, and losing the whole render to it would
    // leave the menu blank.
    if (here.scrollIntoView) here.scrollIntoView({ block: 'nearest' });
    el('burnList').setAttribute('aria-activedescendant', here.id);
  }
}

function chooseImage(name) {
  const sel = el('burnImage');
  if (sel.value === name) return;
  sel.value = name;
  renderPickButton();
  showBurnImageInfo();
}

function openPicker() {
  if (pickable().length === 0) return;
  state.pickAt = Math.max(0, pickMatches().findIndex((f) => f.name === el('burnImage').value));
  el('burnPicker').classList.add('open');
  el('burnPop').hidden = false;
  el('burnPick').setAttribute('aria-expanded', 'true');
  renderPickList();
  el('burnFilter').focus();
  el('burnFilter').select();
}

function closePicker(refocus = true) {
  el('burnPicker').classList.remove('open');
  el('burnPop').hidden = true;
  el('burnPick').setAttribute('aria-expanded', 'false');
  if (refocus) el('burnPick').focus();
}

function pickerOpen() { return !el('burnPop').hidden; }

function movePick(by) {
  const files = pickMatches();
  if (!files.length) return;
  state.pickAt = (state.pickAt + by + files.length) % files.length;
  renderPickList();
}

function wirePicker() {
  el('burnPick').addEventListener('click', () => {
    if (pickerOpen()) closePicker(); else openPicker();
  });
  el('burnPick').addEventListener('keydown', (ev) => {
    if (ev.key === 'ArrowDown' || ev.key === 'ArrowUp') { ev.preventDefault(); openPicker(); }
  });
  el('burnFilter').addEventListener('input', () => { state.pickAt = 0; renderPickList(); });
  el('burnPop').addEventListener('keydown', (ev) => {
    switch (ev.key) {
      case 'ArrowDown': ev.preventDefault(); movePick(1); break;
      case 'ArrowUp': ev.preventDefault(); movePick(-1); break;
      case 'Home': ev.preventDefault(); state.pickAt = 0; renderPickList(); break;
      case 'End': ev.preventDefault(); state.pickAt = pickMatches().length - 1; renderPickList(); break;
      case 'Enter': {
        ev.preventDefault();
        const f = pickMatches()[state.pickAt];
        if (f) { chooseImage(f.name); closePicker(); }
        break;
      }
      case 'Escape': ev.preventDefault(); closePicker(); break;
    }
  });
  // A click anywhere else means "not this one after all".
  document.addEventListener('click', (ev) => {
    if (pickerOpen() && !el('burnPicker').contains(ev.target)) closePicker(false);
  });
}

function renderBurnBox(d) {
  const allowed = state.status && state.status.allowBurn;
  if (!allowed) return;
  // Rebuilding this list every half second is what made the menu impossible
  // to hold open and the row under the cursor flicker, so it is rebuilt only
  // when something it shows has actually changed.
  memo('burn', [d.canBurn, d.burnBlocker, d.busy,
    d.disc && d.disc.present, d.disc && d.disc.erasable,
    el('burnSource').value,
    state.images.map((f) => `${f.name}:${f.size}`).join('|'),
    state.isos.map((f) => `${f.name}:${f.size}`).join('|')],
    () => drawBurnBox(d));
}

function drawBurnBox(d) {
  const blocked = !d.canBurn;
  setHidden(el('burnBlocked'), !blocked);
  setHidden(el('burnFields'), blocked);
  if (blocked) {
    setText(el('burnBlocked'), d.burnBlocker || 'This disc cannot be written to.');
  }
  setHidden(el('btnErase'), !(d.disc && d.disc.present && d.disc.erasable));

  // The library picker only appears when the server has a library.
  const hasISOs = !!(state.status && state.status.isoStore);
  setHidden(el('burnSource').parentElement, !hasISOs);
  if (!hasISOs) el('burnSource').value = 'images';

  // The hidden <select> stays the one place the chosen name lives, so the
  // burn button and the preflight read one field whatever the menu does.
  const sel = el('burnImage');
  const was = sel.value;
  sel.textContent = '';
  const burnable = pickable();
  for (const f of burnable) {
    const o = document.createElement('option');
    o.value = f.name;
    o.textContent = f.name;
    sel.appendChild(o);
  }
  sel.value = burnable.some((f) => f.name === was) ? was : '';
  setDisabled(el('btnBurn'), burnable.length === 0 || !!d.busy);
  el('burnHint').textContent = burnable.length === 0
    ? (el('burnSource').value === 'isos'
        ? 'Nothing in the ISO library is a whole number of 2048-byte sectors.'
        : 'Nothing in the store can be written to a disc: an image has to be a whole '
          + 'number of 2048-byte sectors, and an archive has to be one ripperX can read.')
    : 'Everything that can be checked is checked before the laser is switched on. Afterwards every sector is read back and compared with the image.';
  renderPickButton();
  if (pickerOpen()) renderPickList();
  showBurnImageInfo();
}

// What the chosen image actually is: whether it is really an ISO, which
// disc it needs, and whether a disc written from it will boot. That last
// one cannot be told by looking at the file, and is why this exists - a
// disk image meant for a USB stick is the same shape as an ISO.
async function showBurnImageInfo() {
  const name = el('burnImage').value;
  const source = el('burnSource').value;
  const note = el('burnBoot');
  if (!name) {
    note.hidden = true;
    state.burnInfo = null;
    return;
  }
  if (isArchiveName(name)) {
    // Nothing to ask the server: this one is not an image, and what will
    // happen to it does not depend on what is inside it.
    state.burnInfo = null;
    note.textContent = 'This is an archive. The disc gets the files inside it, ' +
      'as a filesystem \u2014 not the archive file. Every one is read back off the ' +
      'disc afterwards and compared with what went on.';
    note.className = 'note';
    note.hidden = false;
    return;
  }
  // The ISO library listing already looked inside every file, so there is
  // nothing to ask the server: the answer is in hand before the menu
  // closes. The image store is listed without reading anything, so that
  // one is asked about.
  const known = currentImage();
  if (known && known.summary) {
    showImageNote(known);
    return;
  }
  try {
    const info = await api(
      `/api/imageinfo?name=${encodeURIComponent(name)}&source=${encodeURIComponent(source)}`);
    // The menu may have moved on while this was in flight.
    if (el('burnImage').value !== name) return;
    showImageNote(info);
  } catch (e) {
    state.burnInfo = null;
    note.hidden = true;
  }
}

function showImageNote(info) {
  const note = el('burnBoot');
  state.burnInfo = info;
  note.textContent = info.summary;
  // Hatched when it will not boot or is not an image at all; plain when it
  // is what it should be.
  note.className = (info.error || (info.boot && !info.boot.bootable)) ? 'warn' : 'note';
  note.hidden = false;
}

/* ---------- how healthy the disc is ---------- */

// The newest finished scan of this drive, which is what the disc panel
// shows. It comes from the live job list rather than from the database, so
// the result appears the moment the scan ends.
function latestScan(driveID) {
  return state.jobs.find((j) => j.kind === 'scan' && j.drive === driveID && j.scan) || null;
}

const gradeWords = {
  pristine: 'As good as it was made',
  good: 'Healthy',
  worn: 'Wearing out',
  degraded: 'Close to failing',
  failing: 'Already losing data',
  unknown: 'Not measurable',
};

function gradeClass(grade) {
  if (grade === 'failing' || grade === 'degraded') return 'grade bad';
  if (grade === 'worn') return 'grade warn';
  return 'grade';
}

function renderScanPanel(d) {
  const running = state.jobs.some((j) => j.kind === 'scan' && j.drive === d.id && j.state === 'running');
  setDisabled(el('btnScan'), !!d.busy || !d.disc || !d.disc.present);
  setText(el('scanNote'), running
    ? 'Reading every sector. This takes about as long as ripping the disc.'
    : 'Counts the bytes the drive\u2019s error correction could not fix. A disc reads perfectly right up until it does not \u2014 this is what shows the decline while there is still time to copy it.');

  const job = latestScan(d.id);
  const r = job && job.scan;
  // While a scan runs this really does change every tick, and is redrawn.
  // A finished scan is drawn once and then left alone.
  memo('scan', [job && job.id, job && job.state, job && job.done, job && job.total,
    r && r.buckets, r && r.grade, r && r.score, r && r.running], () => drawScan(job));
}

function drawScan(job) {
  const box = el('scanResult');
  setHidden(box, !job);
  if (!job) return;
  const r = job.scan;
  const live = !!r.running;
  box.textContent = '';

  const head = document.createElement('div');
  head.className = 'row';
  head.style.marginBottom = '8px';
  if (live) {
    // No grade while it is still reading: a verdict on half a disc is not
    // a verdict.
    head.appendChild(text('span', 'checking', 'grade'));
    const pctDone = job.total > 0 ? Math.round((job.done / job.total) * 100) : 0;
    head.appendChild(text('span', `${pctDone}%`, 'mono muted'));
    const left = eta(job.etaSeconds);
    if (left) head.appendChild(text('span', left, 'muted'));
  } else {
    head.appendChild(text('span', gradeWords[r.grade] || r.grade, gradeClass(r.grade)));
    if (r.score > 0) head.appendChild(text('span', `${r.score} / 100`, 'mono muted'));
    head.appendChild(text('span', `checked ${when(job.started)}`, 'muted'));
  }
  box.appendChild(head);

  if (!live) {
    box.appendChild(text('p', r.summary, '')).style.margin = '0 0 10px';
  } else {
    const p = text('p', r.c2Supported
      ? 'Counting the bytes the error correction could not fix, as it reads.'
      : 'This disc reports no error flags, so the scan is looking for sectors that will not read at all.', 'muted');
    p.style.margin = '0 0 10px';
    p.style.fontSize = '12px';
    box.appendChild(p);
  }

  box.appendChild(strip(r.map, r.buckets));
  const key = document.createElement('div');
  key.className = 'stripkey';
  // On a disc with no C2 the only marks are the last two, so the legend is
  // trimmed to what this scan could actually have found.
  const legend = r.c2Supported
    ? [['s0', 'clean'], ['s1', 'a few repaired bytes'], ['s2', 'many'],
       ['s3', 'heavy'], ['ss', 'the drive struggled'], ['sx', 'unreadable']]
    : [['s0', 'read cleanly'], ['ss', 'the drive struggled'], ['sx', 'unreadable']];
  if (live) legend.push(['sp', 'not read yet']);
  for (const [cls, label] of legend) {
    const sp = document.createElement('span');
    // The swatch takes the strip's own class, so one CSS rule paints both
    // and they cannot drift apart; its size comes from .stripkey i.
    const sw = document.createElement('i');
    sw.className = cls;
    sp.appendChild(sw);
    sp.appendChild(text('span', label));
    key.appendChild(sp);
  }
  key.appendChild(text('span', 'left is the middle of the disc, right is the outer edge'));
  box.appendChild(key);

  const facts = document.createElement('dl');
  facts.style.marginTop = '12px';
  dl(facts, [
    ['Sectors', r.sectors ? r.sectors.toLocaleString() : null],
    ['Read so far', live && job.total > 0
      ? `${Math.round((job.done / job.total) * 100)}% of the disc` : null],
    ['With errors', r.c2Supported ? `${r.c2Sectors.toLocaleString()} (${pct(r.c2Sectors, r.sectors)})` : null],
    ['Worst sector', r.c2Supported && r.c2Max ? `${r.c2Max} of 2352 bytes` : null],
    ['Unreadable', r.unreadable ? r.unreadable.toLocaleString() : (r.unreadable === 0 ? 'none' : null)],
    ['Where', (r.badRanges || []).length ? r.badRanges.slice(0, 6).join(', ') : null],
    ['Read at', !live && r.avgKbps ? `${Math.round(r.avgKbps)} kB/s average, ${Math.round(r.minKbps)} at its slowest` : null],
    ['Struggled', !live && r.slowStretches
      ? `${r.slowStretches} stretch${r.slowStretches > 1 ? 'es' : ''}, ${r.slowBlocks} blocks` : null],
    ['Took', !live && r.readSeconds ? duration(r.readSeconds) : null],
  ]);
  box.appendChild(facts);

  for (const n of r.notes || []) {
    const p = text('p', n, 'muted');
    p.style.fontSize = '12px';
    p.style.margin = '8px 0 0';
    box.appendChild(p);
  }
}

function pct(n, total) {
  if (!total) return '0%';
  const v = (n / total) * 100;
  return v === 0 ? '0%' : v < 0.01 ? '<0.01%' : `${v.toFixed(2)}%`;
}

// strip draws the damage map: one column per bucket, across the disc.
// Buckets past what has been read are hatched rather than drawn as clean,
// so a scan in progress shows how far it has got instead of implying a
// verdict on the part it has not reached.
function strip(map, scanned) {
  const box = document.createElement('div');
  box.className = 'strip';
  box.setAttribute('role', 'img');
  const read = scanned === undefined ? (map || []).length : scanned;
  box.setAttribute('aria-label', read < (map || []).length
    ? `where the errors are so far; ${Math.round((read / map.length) * 100)}% of the disc read`
    : 'where on the disc the errors are');
  (map || []).forEach((v, i) => {
    const cell = document.createElement('i');
    cell.className = i >= read ? 'sp'
      : v === -1 ? 'sx' : v === -2 ? 'ss'
      : v === 0 ? 's0' : v < 16 ? 's1' : v < 128 ? 's2' : 's3';
    box.appendChild(cell);
  });
  return box;
}

/* ---------- discs checked ---------- */

async function loadDiscs() {
  try {
    const r = await api('/api/discs');
    state.discs = r.discs || [];
    el('discsPanel').hidden = !r.enabled || state.discs.length === 0;
    el('discsNote').textContent = r.enabled
      ? 'Kept in the database, so a disc checked today can be compared with the same disc checked next year. A disc is recognised by its table of contents, not by its name.'
      : '';
    renderDiscs();
  } catch (e) { /* the history is not worth an error banner over */ }
}

function renderDiscs() {
  const box = el('discs');
  box.textContent = '';
  setText(el('discsSummary'), state.discs.length === 1
    ? '1 disc checked before' : `${state.discs.length} discs checked before`);
  for (const d of state.discs) {
    const row = document.createElement('div');
    row.className = 'job';

    const head = document.createElement('div');
    head.className = 'top';
    head.appendChild(text('span', d.label || d.disc, 'what'));
    head.appendChild(text('span', gradeWords[d.latestGrade] || d.latestGrade, gradeClass(d.latestGrade)));
    head.appendChild(text('span', `${d.scans.length} check${d.scans.length > 1 ? 's' : ''}`, 'muted'));
    row.appendChild(head);

    const note = text('p', d.trendNote, 'meta' + (d.trend === 'worse' ? ' trend worse' : ''));
    row.appendChild(note);

    // The scores over time, oldest first, so the direction reads left to
    // right the way a person expects.
    const line = document.createElement('p');
    line.className = 'meta mono';
    line.textContent = d.scans
      .map((sc) => `${when(sc.scannedAt)} ${sc.score}`)
      .join('   \u2192   ');
    row.appendChild(line);
    box.appendChild(row);
  }
}

/* ---------- archives ---------- */

async function loadFormats() {
  try {
    state.formats = (await api('/api/formats')).formats || [];
  } catch (e) {
    state.formats = [];
  }
}

// openArchiveDialog asks which format, then does whatever it was opened
// for. The same dialog serves the folder download and the rip of a
// selection, because the question is the same one.
function openArchiveDialog(purpose) {
  if (state.formats.length === 0 && !purpose.noFormat) return;
  state.archiveFor = purpose;
  el('archiveTitle').textContent = purpose.title;
  el('archiveWhat').textContent = purpose.what;
  el('archiveGo').textContent = purpose.action;
  // Named the same way a rip is: left alone it takes the name of what it
  // came from, and the extension is added for you either way.
  el('archiveName').value = '';
  el('archiveName').placeholder = purpose.suggest || 'taken from the disc';
  el('archiveNameNote').textContent = purpose.nameNote
    || 'Letters of any script, digits, spaces. The extension is added for you.';
  renderWhere(purpose.where || 'browser');

  // One file is written as itself, so there is no format to choose and the
  // question is not asked.
  const box = el('archiveFormats');
  setHidden(box, !!purpose.noFormat);
  setHidden(el('archiveFormatsLabel'), !!purpose.noFormat);
  box.textContent = '';
  state.formats.forEach((f, i) => {
    const label = document.createElement('label');
    label.className = i === 0 ? 'on' : '';
    const radio = document.createElement('input');
    radio.type = 'radio';
    radio.name = 'archiveFormat';
    radio.value = f.id;
    radio.checked = i === 0;
    radio.addEventListener('change', () => {
      for (const l of box.querySelectorAll('label')) l.classList.remove('on');
      label.classList.add('on');
    });
    label.appendChild(radio);
    const body = document.createElement('span');
    body.appendChild(text('span', `${f.name} (${f.extension})`, 'name'));
    body.appendChild(text('span', f.note, 'why'));
    label.appendChild(body);
    box.appendChild(label);
  });
  el('archiveDialog').showModal();
}

function chosenFormat() {
  const picked = el('archiveFormats').querySelector('input:checked');
  return picked ? picked.value : 'zip';
}

// Where the result of a download goes. Pulling a folder off a disc and
// straight onto the share is the same work as sending it to this browser,
// and on a laptop over wifi it is the one that finishes - so it is offered
// beside it rather than hidden in the rip panel.
const destinations = [
  ['browser', 'This browser', 'Sent to the machine you are looking at, as a normal download.'],
  ['store', 'The image store', 'Written on the server, beside the rips. Nothing crosses the network twice.'],
];

function renderWhere(initial) {
  const box = el('archiveWhere');
  box.textContent = '';
  for (const [id, name, why] of destinations) {
    const label = document.createElement('label');
    const radio = document.createElement('input');
    radio.type = 'radio';
    radio.name = 'archiveWhere';
    radio.value = id;
    radio.checked = id === initial;
    if (radio.checked) label.className = 'on';
    radio.addEventListener('change', () => {
      for (const l of box.querySelectorAll('label')) l.classList.remove('on');
      label.classList.add('on');
      // A file going to the store is written by a job, which shows up in
      // the job list; say so while the choice is still being made.
      el('archiveGo').textContent = id === 'store'
        ? 'Save' : (state.archiveFor ? state.archiveFor.action : 'Download');
    });
    label.appendChild(radio);
    const body = document.createElement('span');
    body.appendChild(text('span', name, 'name'));
    body.appendChild(text('span', id === 'store' && state.status ? `${why} ${state.status.store}` : why, 'why'));
    label.appendChild(body);
    box.appendChild(label);
  }
}

function chosenWhere() {
  const picked = el('archiveWhere').querySelector('input:checked');
  return picked ? picked.value : 'browser';
}

function chosenName() {
  return el('archiveName').value.trim();
}

// A disc with data on it usually cannot be burned and very often can be
// added to. That is the case this panel exists for: a DVD with a third of a
// gigabyte going spare is not a disc to throw away.
const appendPicked = new Set();

function renderAppendBox(d) {
  const allowed = state.status && state.status.allowBurn;
  if (!allowed) return;
  const free = (d.disc && d.disc.writableBytes) || 0;
  // Ticking a file and having the tick vanish half a second later is what
  // rebuilding this list on every snapshot did.
  memo('append', [d.canAppend, d.appendBlocker, d.busy, free,
    el('appendFilter').value,
    state.images.map((f) => `${f.name}:${f.size}`).join('|')],
    () => drawAppendBox(d, free));
  updateAppend(free);
}

function drawAppendBox(d, free) {
  const blocked = !d.canAppend;
  setHidden(el('appendBlocked'), !blocked);
  setHidden(el('appendFields'), blocked);
  if (blocked) {
    setText(el('appendBlocked'), d.appendBlocker || 'Files cannot be added to this disc.');
    return;
  }

  const matched = matching(state.images, el('appendFilter').value);
  const shown = matched.slice(0, imagePage);
  setText(el('appendCount'), countLine(shown.length, matched.length, state.images.length, 'images'));
  sync(el('appendRows'), shown, (f) => f.name, (f) => {
    const tr = document.createElement('tr');
    const tick = document.createElement('td');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.setAttribute('aria-label', `add ${f.name}`);
    box.addEventListener('change', () => {
      if (box.checked) appendPicked.add(f.name); else appendPicked.delete(f.name);
      updateAppend((state.detail && state.detail.disc && state.detail.disc.writableBytes) || 0);
    });
    tick.appendChild(box);
    tr.appendChild(tick);
    tr.appendChild(text('td', '', 'wrapname'));
    tr.appendChild(text('td', '', 'num'));
    return tr;
  }, (tr, f) => {
    const box = tr.querySelector('input');
    if (box.checked !== appendPicked.has(f.name)) box.checked = appendPicked.has(f.name);
    setText(tr.children[1], f.name);
    setText(tr.children[2], bytes(f.size));
  });
  // A file that has been deleted from the store since it was ticked must
  // not stay in the selection and then fail at the far end.
  for (const name of Array.from(appendPicked)) {
    if (!state.images.some((f) => f.name === name)) appendPicked.delete(name);
  }
}

function appendSelectedBytes() {
  return state.images
    .filter((f) => appendPicked.has(f.name))
    .reduce((n, f) => n + f.size, 0);
}

function updateAppend(free) {
  const picked = appendPicked.size;
  const want = appendSelectedBytes();
  // The server keeps two megabytes back for the new session's own
  // structures; the page says the same thing so the button does not offer
  // something the server will refuse.
  const overhead = 2 * 1024 * 1024;
  const fits = want + overhead <= free;
  el('btnAppend').disabled = picked === 0 || !fits;
  el('appendRoom').textContent = picked === 0
    ? `${bytes(free)} free on this disc. Tick what to add.`
    : fits
      ? `${bytes(want)} selected, ${bytes(free)} free.`
      : `${bytes(want)} selected, which will not fit in the ${bytes(free)} left.`;
}

/* ---------- files ---------- */

function renderFilesTab(d) {
  const can = d.canBrowse && d.disc && d.disc.present;
  el('filesNone').hidden = can;
  el('filesBody').hidden = !can;
  if (!can) {
    el('filesNone').textContent = d.browseError ||
      'This disc has no filesystem ripperX can read. An audio CD has none at all; a disc written only in UDF is not supported.';
  }
}

async function loadDir(path) {
  const id = state.selected;
  if (!id) return;
  try {
    const r = await api(`/api/drives/${encodeURIComponent(id)}/browse?path=${encodeURIComponent(path)}`);
    state.path = r.path;
    state.entries = r.entries;
    fail('');
    renderFiles(r);
  } catch (e) {
    state.entries = [];
    el('fileRows').textContent = '';
    fail(String(e.message || e));
  }
}

function renderFiles(r) {
  const id = state.selected;
  // Breadcrumbs: every level is a way back, which is quicker than Up twice.
  const bc = el('bread');
  bc.textContent = '';
  const parts = r.path === '/' ? [] : r.path.replace(/^\//, '').split('/');
  const root = text('button', r.volume.volumeId || 'disc', 'link');
  root.type = 'button';
  root.addEventListener('click', () => loadDir('/'));
  bc.appendChild(root);
  let acc = '';
  parts.forEach((p, i) => {
    acc += '/' + p;
    bc.appendChild(text('span', ' / '));
    if (i === parts.length - 1) {
      bc.appendChild(text('span', p));
    } else {
      const here = acc;
      const b = text('button', p, 'link');
      b.type = 'button';
      b.addEventListener('click', () => loadDir(here));
      bc.appendChild(b);
    }
  });

  el('btnUp').disabled = !r.parent;
  el('btnUp').onclick = () => loadDir(r.parent || '/');

  const rows = el('fileRows');
  rows.textContent = '';
  for (const e of r.entries) {
    rows.appendChild(fileRow(id, e));
  }
  el('selAll').checked = false;
  updatePicked();
}

function fileRow(id, e) {
  const tr = document.createElement('tr');

  const tick = document.createElement('td');
  const box = document.createElement('input');
  box.type = 'checkbox';
  box.checked = state.picked.has(e.path);
  box.setAttribute('aria-label', `rip ${e.name}`);
  box.addEventListener('change', () => {
    if (box.checked) state.picked.add(e.path); else state.picked.delete(e.path);
    updatePicked();
  });
  tick.appendChild(box);
  tr.appendChild(tick);

  const nameCell = document.createElement('td');
  nameCell.className = 'wrapname';
  if (e.isDir) {
    const b = text('button', e.name + '/', 'link');
    b.type = 'button';
    b.addEventListener('click', () => loadDir(e.path));
    nameCell.appendChild(b);
  } else {
    nameCell.appendChild(text('span', e.name));
    if (e.symlinkTarget) nameCell.appendChild(text('span', ` → ${e.symlinkTarget}`, 'muted'));
  }
  tr.appendChild(nameCell);

  tr.appendChild(text('td', e.isDir ? '' : bytes(e.size), 'num'));
  tr.appendChild(text('td', when(e.modTime), 'num drop-col'));

  const act = document.createElement('td');
  act.className = 'act';
  if (!e.isDir) {
    const file = `/api/drives/${encodeURIComponent(id)}/file?path=${encodeURIComponent(e.path)}`;
    if (e.playable) {
      const play = text('button', 'Play');
      play.type = 'button';
      play.addEventListener('click', () => togglePlayer(tr, e, file));
      act.appendChild(play);
    }
    // One control rather than two. A file can go to this browser or into
    // the image store, and either way it can be called something else on
    // the way out - and that is the same question for a file, a folder and
    // a ticked selection, so it is the same dialog.
    const take = text('button', 'Take\u2026');
    take.type = 'button';
    take.title = 'Download it, or write it into the image store';
    take.addEventListener('click', () => openArchiveDialog({
      title: 'Take this file',
      what: `${e.path} \u2014 ${bytes(e.size)}.`,
      action: 'Download',
      suggest: e.name,
      noFormat: true,
      nameNote: 'Left empty it keeps the name it has on the disc.',
      run: (format, where, name) => where === 'store'
        ? startRip('files', { paths: [e.path], name })
        : downloadDiscFile(id, e.path, name),
    }));
    act.appendChild(take);
  } else {
    const b = text('button', 'Download\u2026');
    b.type = 'button';
    b.addEventListener('click', () => openArchiveDialog({
      title: 'Take this folder',
      what: `${e.path} \u2014 everything under it, as one archive.`,
      action: 'Download',
      suggest: e.name,
      run: (format, where, name) => takeFolder(id, e.path, format, where, name),
    }));
    act.appendChild(b);
  }
  tr.appendChild(act);
  return tr;
}

// togglePlayer opens a player underneath the row rather than in a dialog:
// the file stays where it was found, and a second click puts it away.
function togglePlayer(tr, e, url) {
  if (tr.nextSibling && tr.nextSibling.dataset && tr.nextSibling.dataset.player === '1') {
    tr.nextSibling.remove();
    return;
  }
  const row = document.createElement('tr');
  row.dataset.player = '1';
  const cell = document.createElement('td');
  cell.colSpan = 5;
  const video = e.type.startsWith('video/');
  const media = document.createElement(video ? 'video' : 'audio');
  media.controls = true;
  media.preload = 'none';
  media.src = url + '&inline=1';
  if (video) { media.style.width = '100%'; media.style.maxWidth = '640px'; }
  cell.appendChild(media);
  const note = text('p', `${e.type} · streamed from the disc; seeking seeks the laser.`, 'muted');
  note.style.fontSize = '11px';
  note.style.margin = '6px 0 0';
  cell.appendChild(note);
  row.appendChild(cell);
  tr.after(row);
  media.play().catch(() => { /* the browser would rather the user pressed play */ });
}

// downloadArchive hands the browser a URL rather than fetching it: the
// archive is produced as the disc is read and can be far larger than
// anything worth holding in a page.
function downloadArchive(id, path, format, name) {
  window.location.href =
    `/api/drives/${encodeURIComponent(id)}/archive?path=${encodeURIComponent(path)}` +
    `&format=${encodeURIComponent(format)}${nameParam(name)}`;
}

// nameParam carries the name the dialog asked for. The server makes it safe
// and keeps the extension, so this is a preference rather than a path.
function nameParam(name) {
  return name ? `&name=${encodeURIComponent(name)}` : '';
}

// takeFolder is the same folder either way: the browser downloads it, or
// the server writes it into the image store as a rip.
function takeFolder(id, path, format, where, name) {
  if (where === 'store') startRip('files', { paths: [path], format, name });
  else downloadArchive(id, path, format, name);
}

// downloadPicked sends the ticked files to the browser. Nothing on the
// server streams an arbitrary set of paths as one archive - a folder or a
// file is what a URL can name - and twenty downloads at once is not a
// kindness, so anything else is said plainly rather than half done.
function downloadPicked(name) {
  const paths = Array.from(state.picked);
  if (paths.length === 1) {
    downloadDiscFile(state.selected, paths[0], name);
    return;
  }
  fail('A browser download takes one file or one whole folder at a time. Tick a single file, ' +
    'use Download on the folder itself, or save the selection to the image store.');
}

function downloadDiscFile(id, path, name) {
  window.location.href =
    `/api/drives/${encodeURIComponent(id)}/file?path=${encodeURIComponent(path)}${nameParam(name)}`;
}

function updatePicked() {
  const n = state.picked.size;
  el('btnRipSel').disabled = n === 0;
  el('selNote').textContent = n === 0 ? '' :
    `${n} selected${n > 1 ? ' - they will be written as one .tar' : ''}`;
}

/* ---------- audio ---------- */

function renderAudioTab(d) {
  const tracks = (d.disc && d.disc.tracks || []).filter((t) => t.audio);
  // Rebuilding this rebuilt the <audio> elements, which stopped whatever was
  // playing and lost its position - twice a second, for as long as the tab
  // was open.
  memo('audio', [d.id, d.canRipAudio, d.disc && d.disc.present,
    tracks.map((t) => `${t.number}:${t.durationSeconds}`).join('|')],
    () => drawAudioTab(d, tracks));
}

function drawAudioTab(d, tracks) {
  const can = tracks.length > 0;
  setHidden(el('audioNone'), can);
  setHidden(el('audioBody'), !can);
  if (!can) {
    setText(el('audioNone'), d.disc && d.disc.present
      ? 'There are no audio tracks on this disc.'
      : 'There is no disc in this drive.');
    return;
  }
  const id = d.id;
  el('btnM3U').href = `/api/drives/${encodeURIComponent(id)}/playlist.m3u`;
  el('btnRipAudio').disabled = !d.canRipAudio;

  const rows = el('trackRows');
  rows.textContent = '';
  for (const t of tracks) {
    const tr = document.createElement('tr');
    tr.appendChild(text('td', `Track ${String(t.number).padStart(2, '0')}` +
      (t.preEmphasis ? ' (pre-emphasis)' : '')));
    tr.appendChild(text('td', duration(t.durationSeconds), 'num'));

    const play = document.createElement('td');
    const audio = document.createElement('audio');
    audio.controls = true;
    audio.preload = 'none';
    audio.src = `/api/drives/${encodeURIComponent(id)}/audio/${t.number}.wav`;
    play.appendChild(audio);
    tr.appendChild(play);

    const act = document.createElement('td');
    act.className = 'act';
    const a = document.createElement('a');
    a.className = 'btn';
    a.href = audio.src;
    a.textContent = 'Download .wav';
    act.appendChild(a);
    tr.appendChild(act);
    rows.appendChild(tr);
  }
}

/* ---------- what the drive can do ---------- */

function renderDriveTab(d) {
  const c = d.capabilities;
  memo('drive', [d.id, d.error, c && c.currentProfileName, c && c.currentReadSpeedKb,
    c && (c.can || []).length, c && (c.cannot || []).length], () => drawDriveTab(d));
}

function drawDriveTab(d) {
  const c = d.capabilities;
  if (!c) {
    el('driveFacts').textContent = '';
    el('can').textContent = '';
    el('cannot').textContent = '';
    el('capNotes').textContent = d.error || 'This drive has not said what it is.';
    return;
  }
  const x = (kb) => (kb ? `${(kb / 176).toFixed(0)}x (${kb} kB/s)` : null);
  dl(el('driveFacts'), [
    ['Model', `${c.info.vendor} ${c.info.product}`],
    ['Firmware', c.info.version],
    ['Serial', c.serialNumber || null],
    ['Loading', c.loadingMechanism],
    ['Buffer', c.bufferKb ? `${c.bufferKb} kB` : null],
    ['Reads up to', x(c.maxReadSpeedKb)],
    ['Reading at', x(c.currentReadSpeedKb)],
    ['Writes up to', x(c.maxWriteSpeedKb)],
    ['In the drive', c.currentProfileName],
  ]);

  const fill = (node, list) => {
    node.textContent = '';
    for (const line of list || []) node.appendChild(text('li', line));
  };
  fill(el('can'), c.can);
  fill(el('cannot'), c.cannot);
  el('capNotes').textContent = (c.notes || []).join(' ');
}

/* ---------- jobs ---------- */

// Running jobs are what anyone is looking at; finished ones are a record,
// and a record of forty of them does not belong above the thing you are
// doing. So the running ones are listed, and the rest are behind a count.
function renderJobs() {
  const running = state.jobs.filter((j) => j.state === 'running');
  const done = state.jobs.filter((j) => j.state !== 'running');

  setHidden(el('noJobs'), running.length > 0);
  sync(el('jobs'), running, (j) => j.id, createJob, updateJob);

  setHidden(el('doneMore'), done.length === 0);
  setText(el('doneSummary'), done.length === 1 ? '1 finished job'
    : `${done.length} finished jobs`);
  sync(el('doneJobs'), done, (j) => j.id, createJob, updateJob);
}

// A job is built once and then only written into. Its shape does not depend
// on its progress: the bar, the percentage and the line of figures are
// always there, so a job at 4% and the same job at 100% are the same height
// and nothing below them moves as it runs.
function createJob(j) {
  const div = document.createElement('div');
  div.className = 'job';

  const top = document.createElement('div');
  top.className = 'top';
  top.appendChild(text('span', '', 'what'));
  top.appendChild(text('span', '', 'pct'));
  const stop = text('button', 'Stop');
  stop.type = 'button';
  stop.className = 'stop';
  stop.addEventListener('click', async () => {
    stop.disabled = true;
    try { await post(`/api/jobs/${encodeURIComponent(j.id)}/cancel`); }
    catch (e) { fail(String(e.message || e)); }
  });
  top.appendChild(stop);
  div.appendChild(top);

  const bar = document.createElement('div');
  bar.className = 'bar';
  bar.appendChild(document.createElement('i'));
  div.appendChild(bar);

  div.appendChild(text('p', '', 'meta'));
  div.appendChild(text('p', '', 'meta says'));
  div.appendChild(text('p', '', 'meta jobfail'));
  div.appendChild(text('p', '', 'meta mono sum'));
  const targets = document.createElement('div');
  targets.className = 'targets';
  div.appendChild(targets);
  return div;
}

function updateJob(div, j) {
  const running = j.state === 'running';
  setText(div.querySelector('.what'), j.label);
  setHidden(div.querySelector('.stop'), !running);

  const bar = div.querySelector('.bar');
  setHidden(bar, !(j.total > 0));
  setClass(bar, 'done', j.state === 'done');
  setClass(bar, 'bad', j.state === 'failed');
  const share = j.total > 0 ? Math.min(100, (j.done / j.total) * 100) : 0;
  setWidth(bar.firstChild, `${share.toFixed(1)}%`);
  setText(div.querySelector('.pct'), j.total > 0 ? `${Math.round(share)}%` : j.state);

  const bits = [];
  if (j.phase) bits.push(j.phase);
  if (j.total > 0) bits.push(`${bytes(j.done)} of ${bytes(j.total)}`);
  if (j.bytesPerSec > 0) bits.push(rate(j.bytesPerSec));
  if (running) {
    bits.push(`${since(j.started)} so far`);
    const left = eta(j.etaSeconds);
    if (left) bits.push(left);
  } else {
    bits.push(`${j.state} · ${when(j.started)}`);
  }
  if (j.badSectors > 0) {
    bits.push(`${j.badSectors} unreadable sector${j.badSectors > 1 ? 's' : ''}` +
      (j.badRanges && j.badRanges.length ? ` at ${j.badRanges.slice(0, 4).join(', ')}` : ''));
  }
  setText(div.querySelector('.meta'), bits.join(' · '));

  const says = div.querySelector('.says');
  setText(says, j.message || '');
  setHidden(says, !j.message);

  const bad = div.querySelector('.jobfail');
  setText(bad, j.error || '');
  setHidden(bad, !j.error);

  const sum = div.querySelector('.sum');
  setText(sum, j.sha256 ? `SHA-256 ${j.sha256}` : '');
  setHidden(sum, !j.sha256);

  sync(div.querySelector('.targets'), j.targets || [], (t) => t, (t) => {
    const a = document.createElement('a');
    a.className = 'btn';
    a.href = `/api/images/${encodeURIComponent(t)}`;
    a.textContent = t;
    return a;
  }, () => {});
}

/* ---------- images ---------- */

function upperFirst(s) {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

// Which naming scheme the disc's names came from. It is worth showing
// because it is the difference between a disc whose names survive a copy
// and one whose names were flattened to 8.3 by its author. UDF has no such
// distinction: it has had real names since it was written.
function namingScheme(vol) {
  if (vol.format === 'UDF') return 'UDF, any script';
  if (vol.joliet) return 'Joliet';
  if (vol.rockRidge) return 'Rock Ridge';
  return 'ISO 9660 only';
}

async function loadISOs() {
  try {
    const r = await api('/api/isos');
    state.isos = r.files || [];
  } catch (e) {
    state.isos = [];
  }
}

async function loadImages() {
  try {
    const r = await api('/api/images');
    state.images = r.files || [];
    el('storeLine').textContent = `${r.store}${r.free ? ` · ${bytes(r.free)} free` : ''}`;
    el('storeFoot').textContent = r.store;
    renderImages();
    if (state.detail) renderBurnBox(state.detail);
  } catch (e) {
    fail(String(e.message || e));
  }
}

// A store can hold hundreds of images. Two things follow: nothing is found
// by scrolling, so there is a filter; and a browser asked to lay out a
// thousand rows at once stops responding while it does, so only the first
// hundred are drawn until the rest are asked for.
const imagePage = 10;

function matching(files, query) {
  const q = String(query || '').trim().toLowerCase();
  if (!q) return files;
  const words = q.split(/\s+/);
  return files.filter((f) => {
    const hay = f.name.toLowerCase();
    return words.every((w) => hay.includes(w));
  });
}

// countLine says what is being shown out of what there is, which is the
// thing a filter has to say or nobody trusts it.
function countLine(shown, matched, total, what) {
  if (total === 0) return '';
  if (matched < total) {
    return shown < matched
      ? `${shown} of ${matched} matching, ${total} ${what} in all`
      : `${matched} of ${total} ${what}`;
  }
  return shown < total ? `${shown} of ${total} ${what}` : `${total} ${what}`;
}

function renderImages() {
  const rows = el('imageRows');
  rows.textContent = '';
  el('noImages').hidden = state.images.length > 0;

  const matched = matching(state.images, el('imageFilter').value);
  const shown = state.imagesAll ? matched : matched.slice(0, imagePage);
  setText(el('imageCount'), countLine(shown.length, matched.length, state.images.length, 'images'));
  setHidden(el('imageMore'), state.imagesAll || matched.length <= shown.length);
  setText(el('imageMore'), `Show the other ${matched.length - shown.length}`);

  for (const f of shown) {
    const tr = document.createElement('tr');
    tr.appendChild(text('td', f.name, 'wrapname'));
    tr.appendChild(text('td', bytes(f.size), 'num'));

    const act = document.createElement('td');
    act.className = 'act';

    const a = document.createElement('a');
    a.className = 'btn';
    a.href = `/api/images/${encodeURIComponent(f.name)}`;
    a.textContent = 'Download';
    act.appendChild(a);

    // A raw image can be turned into something burnable without going back
    // to the disc, which is the only reason this button is here.
    if (f.size % 2352 === 0 && f.size % 2048 !== 0) {
      const conv = text('button', 'To .iso');
      conv.type = 'button';
      conv.addEventListener('click', async () => {
        conv.disabled = true;
        try { await post('/api/convert', { name: f.name }); }
        catch (e) { fail(String(e.message || e)); conv.disabled = false; }
      });
      act.appendChild(conv);
    }

    const del = text('button', 'Delete');
    del.type = 'button';
    del.addEventListener('click', async () => {
      if (!window.confirm(`Delete ${f.name}? This cannot be undone.`)) return;
      del.disabled = true;
      try {
        await api(`/api/images/${encodeURIComponent(f.name)}`, { method: 'DELETE' });
        await loadImages();
      } catch (e) { fail(String(e.message || e)); del.disabled = false; }
    });
    act.appendChild(del);

    tr.appendChild(act);
    rows.appendChild(tr);
  }
}

// Uploading goes through XMLHttpRequest rather than fetch for one reason:
// it reports how far it has got, and an image being uploaded to burn is
// big enough that a bar matters.
function upload(file) {
  if (!file) return;
  const form = new FormData();
  form.append('file', file);
  const xhr = new XMLHttpRequest();
  const bar = el('upBar');
  const fillBar = bar.querySelector('i');
  bar.hidden = false;
  el('upNote').hidden = false;
  el('upNote').textContent = `uploading ${file.name}`;

  xhr.upload.addEventListener('progress', (ev) => {
    if (ev.lengthComputable) fillBar.style.width = `${(ev.loaded / ev.total) * 100}%`;
  });
  xhr.addEventListener('load', async () => {
    bar.hidden = true;
    fillBar.style.width = '0';
    if (xhr.status === 401) { window.location.replace('/login'); return; }
    let body = null;
    try { body = JSON.parse(xhr.responseText); } catch (e) { /* not JSON */ }
    if (xhr.status >= 400) {
      el('upNote').textContent = (body && body.error) || `upload failed: HTTP ${xhr.status}`;
      return;
    }
    el('upNote').textContent = `${body.file.name} stored · SHA-256 ${body.sha256}`;
    await loadImages();
  });
  xhr.addEventListener('error', () => {
    bar.hidden = true;
    el('upNote').textContent = 'the upload did not finish';
  });
  xhr.open('POST', '/api/upload');
  xhr.send(form);
}

/* ---------- actions ---------- */

async function startRip(kind, extra) {
  const d = state.detail;
  if (!d) return;
  const body = Object.assign({
    drive: d.id,
    kind,
    name: el('ripName').value.trim(),
    speedKb: Number(el('ripSpeed').value) || 0,
  }, extra || {});
  if (kind === 'iso') body.length = el('ripLength').value;
  try {
    await post('/api/rip', body);
    fail('');
  } catch (e) {
    fail(String(e.message || e));
  }
}

function wireActions() {
  // The pane buttons are built from what the disc can do, so they wire
  // themselves as they are created; there is nothing static to bind here.
  el('ripKind').addEventListener('change', () => {
    el('ripLengthField').hidden = el('ripKind').value !== 'iso';
  });
  el('btnRip').addEventListener('click', () => startRip(el('ripKind').value));
  el('btnRipAudio').addEventListener('click', () => startRip('audio'));
  el('btnRipSel').addEventListener('click', () => {
    const n = state.picked.size;
    if (n === 1) {
      // One file is written as itself, so there is no format to choose.
      startRip('files', { paths: Array.from(state.picked) });
      return;
    }
    openArchiveDialog({
      title: 'Take what is ticked',
      what: `${n} selected items, wrapped in one archive.`,
      action: 'Save',
      where: 'store',
      suggest: 'taken from the disc\u2019s label',
      run: (format, where, name) => where === 'store'
        ? startRip('files', { paths: Array.from(state.picked), format, name })
        : downloadPicked(name),
    });
  });

  el('btnArchive').addEventListener('click', () => openArchiveDialog({
    title: 'Take this folder',
    what: `${state.path} \u2014 everything under it, as one archive.`,
    action: 'Download',
    suggest: state.path === '/' ? 'the disc\u2019s label' : state.path.split('/').pop(),
    run: (format, where, name) => takeFolder(state.selected, state.path, format, where, name),
  }));

  el('archiveDialog').addEventListener('close', () => {
    const dlg = el('archiveDialog');
    const purpose = state.archiveFor;
    state.archiveFor = null;
    if (dlg.returnValue === 'ok' && purpose) purpose.run(chosenFormat(), chosenWhere(), chosenName());
  });

  el('btnScan').addEventListener('click', async () => {
    const d = state.detail;
    if (!d) return;
    try {
      await post('/api/scan', { drive: d.id, speedKb: Number(el('scanSpeed').value) || 0 });
      fail('');
    } catch (e) { fail(String(e.message || e)); }
  });

  el('selAll').addEventListener('change', (ev) => {
    for (const e of state.entries) {
      if (ev.target.checked) state.picked.add(e.path); else state.picked.delete(e.path);
    }
    for (const box of el('fileRows').querySelectorAll('input[type=checkbox]')) {
      box.checked = ev.target.checked;
    }
    updatePicked();
  });

  el('burnSource').addEventListener('change', () => {
    if (state.detail) renderBurnBox(state.detail);
  });
  wirePicker();

  el('imageFilter').addEventListener('input', () => {
    state.imagesAll = false;
    renderImages();
  });
  el('imageMore').addEventListener('click', () => {
    state.imagesAll = true;
    renderImages();
  });
  el('appendFilter').addEventListener('input', () => {
    // The rows are memoised against the image list, so a filter that is not
    // part of that has to say the drawing is out of date.
    forget('append');
    if (state.detail) renderAppendBox(state.detail);
  });

  el('btnBurn').addEventListener('click', async () => {
    const d = state.detail;
    const image = el('burnImage').value;
    if (!d || !image) return;
    const dummy = el('burnDummy').checked;
    const unpack = isArchiveName(image);
    let what;
    if (unpack) {
      what = `Write the files inside ${image} to the disc in ${d.id}?\n\n` +
        'The disc will hold the files, not the archive. This cannot be undone.';
    } else if (dummy) {
      what = `Rehearse writing ${image} in ${d.id}? The laser stays off and the disc is untouched.`;
    } else {
      what = `Write ${image} to the disc in ${d.id}?\n\nThis cannot be undone: whatever is on the disc now is gone.`;
    }
    // If it will not boot, say so here rather than after the disc is spent.
    const info = state.burnInfo;
    if (!dummy && info && info.boot && !info.boot.bootable) {
      what += `\n\n${upperFirst(info.boot.why || 'This image will not boot.')}`;
    }
    if (!window.confirm(what)) return;
    try {
      await post('/api/burn', {
        drive: d.id,
        image,
        source: el('burnSource').value,
        speedX: Number(el('burnSpeed').value) || 0,
        dummy: dummy && !unpack,
        unpack,
        verify: el('burnVerify').checked,
      });
      fail('');
    } catch (e) { fail(String(e.message || e)); }
  });

  el('btnAppend').addEventListener('click', async () => {
    const d = state.detail;
    if (!d || appendPicked.size === 0) return;
    const names = Array.from(appendPicked);
    const folder = el('appendFolder').value.trim();
    const closing = el('appendClose').checked;
    let what = `Add ${names.length} file${names.length > 1 ? 's' : ''} to the disc in ${d.id}?\n\n` +
      'Nothing already on it is erased.';
    if (closing) {
      what += '\n\nThe disc will then be closed, and nothing can ever be added to it again. That cannot be undone.';
    }
    if (!window.confirm(what)) return;
    try {
      await post('/api/append', {
        drive: d.id,
        names,
        folder,
        verify: el('appendVerify').checked,
        close: closing,
      });
      appendPicked.clear();
      fail('');
    } catch (e) { fail(String(e.message || e)); }
  });

  el('btnErase').addEventListener('click', async () => {
    const d = state.detail;
    if (!d) return;
    if (!window.confirm(`Erase the disc in ${d.id}? Everything on it is lost.`)) return;
    try { await post('/api/erase', { drive: d.id }); fail(''); }
    catch (e) { fail(String(e.message || e)); }
  });

  el('file').addEventListener('change', (ev) => {
    upload(ev.target.files[0]);
    ev.target.value = '';
  });
  const drop = el('drop');
  for (const name of ['dragenter', 'dragover']) {
    drop.addEventListener(name, (ev) => { ev.preventDefault(); drop.classList.add('over'); });
  }
  for (const name of ['dragleave', 'drop']) {
    drop.addEventListener(name, () => drop.classList.remove('over'));
  }
  drop.addEventListener('drop', (ev) => {
    ev.preventDefault();
    if (ev.dataTransfer.files.length) upload(ev.dataTransfer.files[0]);
  });

  el('btnLogout').addEventListener('click', async () => {
    try { await post('/api/logout'); } catch (e) { /* signing out is best effort */ }
    window.location.replace('/login');
  });
}

/* ---------- the stream ---------- */

function applySnapshot(snap) {
  state.drives = snap.drives || [];
  state.jobs = snap.jobs || [];
  renderDrives();
  renderJobs();

  const d = driveByID(state.selected);
  if (!d) return;
  // A disc swapped in the selected drive invalidates the volume, the
  // listing and the tabs built from them.
  const key = discKey(d);
  if (key !== state.discKey) {
    state.discKey = key;
    state.path = '/';
    state.picked.clear();
    state.entries = [];
    forgetDisc();
    loadDetail();
    return;
  }
  // The shallow view still carries what the buttons are enabled by, so a
  // drive that has just been taken by a job greys out at once.
  if (state.detail) {
    Object.assign(state.detail, {
      disc: d.disc, busy: d.busy, canBurn: d.canBurn, burnBlocker: d.burnBlocker,
      canAppend: d.canAppend, appendBlocker: d.appendBlocker,
      canRipIso: d.canRipIso, canRipImg: d.canRipImg, canRipAudio: d.canRipAudio,
    });
    renderWork();
  }
}

let refreshImagesOn = new Set();

function watchJobsForImages() {
  // Any job that has just finished may have written a file, so the image
  // list is reloaded once per finish rather than on a timer.
  const finished = new Set(state.jobs.filter((j) => j.state !== 'running').map((j) => j.id));
  let changed = false;
  for (const id of finished) if (!refreshImagesOn.has(id)) changed = true;
  refreshImagesOn = finished;
  if (changed) {
    loadImages();
    loadDiscs();
  }
}

function connect() {
  const es = new EventSource('/api/events');
  es.addEventListener('open', () => {
    el('conn').className = 'conn live';
    setText(el('conn'), 'live');
    el('conn').title = 'Every browser watching this server sees the same drives and the same jobs.';
  });
  es.addEventListener('message', (ev) => {
    try {
      const snap = JSON.parse(ev.data);
      applySnapshot(snap);
      watchJobsForImages();
    } catch (e) { /* a malformed frame is not worth tearing the page down for */ }
  });
  es.addEventListener('error', () => {
    el('conn').className = 'conn off';
    setText(el('conn'), 'reconnecting');
  });
}

async function main() {
  wireActions();
  try {
    state.status = await api('/api/status');
  } catch (e) {
    fail(String(e.message || e));
    return;
  }
  const s = state.status;
  el('btnLogout').hidden = !s.authOn;
  el('burnerFoot').textContent = s.burner ? `${s.burnerKind} (${s.burner})` : 'a burner program, once one is installed';
  el('status').textContent =
    `ripperX ${s.version} · ${s.drives} drive${s.drives === 1 ? '' : 's'} · ` +
    `images in ${s.store}` +
    (s.allowBurn ? ` · burning with ${s.burnerKind}` : ' · read only');
  if (!s.allowBurn && s.burner === '') {
    el('warning').textContent =
      'No burner program is installed, so discs can be read but not written. Install xorriso to burn.';
    el('warning').hidden = false;
  }
  el('storeFoot').textContent = s.store;
  await Promise.all([loadImages(), loadISOs(), loadFormats(), loadDiscs()]);
  connect();
}

main();
