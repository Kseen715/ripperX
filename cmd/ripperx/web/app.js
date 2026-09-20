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
  titles: [],        // what a DVD's own index says is on it
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
  // The last directory listing, kept so a folder size arriving later can be
  // drawn into it without asking the drive for the listing again.
  browse: null,
  // How big each folder turned out to be, by path. A folder's size costs a
  // walk of its whole tree, so it is asked for once and then remembered for
  // as long as the disc is in the drive.
  dirSizes: new Map(),
  // The last recorded check of a disc, by its fingerprint, read out of the
  // database rather than out of a job. It is what a page that has just been
  // reloaded - or a server that has just been restarted - shows instead of
  // pretending the disc was never checked.
  storedScans: new Map(),
};

/* ---------- small helpers ---------- */

function bytes(n) {
  if (n === null || n === undefined) return '-';
  if (n < 1024) return `${n} ${t('unit.b')}`;
  const units = ['unit.kb', 'unit.mb', 'unit.gb', 'unit.tb'];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${t(units[i])}`;
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
  if (seconds < 45) return t('eta.underMinute');
  const mins = Math.round(seconds / 60);
  if (mins < 60) return tn('eta.minutes', mins);
  return t('eta.hours', { h: Math.floor(mins / 60), m: mins % 60 });
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

/* ---------- asking first ---------- */

// ask() is window.confirm, in the page's own clothes and with the page's
// own guarantees.
//
// The browser's confirm is not one of them. After a few dialogs a browser
// offers to stop a page creating more, and once that is on, confirm returns
// false without showing anything - so "are you sure?" becomes a button that
// silently does nothing, and the user is left pressing Delete at a list
// that will not change. It is also the one piece of this page that cannot
// be styled, cannot say more than one line, and on a phone appears at the
// top of the screen away from the thumb that asked for it.
//
// Cancel holds the focus, so a stray Enter or a double tap lands on the
// safe one. Every caller here is asking about something that cannot be
// undone.
function ask({ title, what, action, danger }) {
  const dlg = el('confirmDialog');
  setText(el('confirmTitle'), title || t('confirm.title'));
  setText(el('confirmWhat'), what || '');
  setText(el('confirmGo'), action || t('common.yes'));
  setText(el('confirmWarn'), danger || '');
  setHidden(el('confirmWarn'), !danger);
  return new Promise((resolve) => {
    const done = () => {
      dlg.removeEventListener('close', done);
      resolve(dlg.returnValue === 'ok');
    };
    dlg.addEventListener('close', done);
    dlg.returnValue = '';
    dlg.showModal();
    el('confirmNo').focus();
  });
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
    throw new Error(t('error.signedOut'));
  }
  if (!r.ok) {
    let msg = t('error.http', { status: r.status });
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
  // The sizes belonged to the tree on the disc that has just left.
  state.dirSizes.clear();
  state.browse = null;
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
    tray.appendChild(trayButton(d, 'eject', t('drives.open')));
    tray.appendChild(trayButton(d, 'load', t('drives.close')));

    const refresh = text('button', t('drives.reread'));
    refresh.type = 'button';
    refresh.dataset.act = 'refresh';
    refresh.title = t('drives.rereadWhy');
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
  if (d.busy) { setText(chip, t('drives.working')); chip.className = 'chip busy'; }
  else if (!d.disc || !d.disc.present) { setText(chip, t('drives.empty')); chip.className = 'chip none'; }
  else { setText(chip, d.disc.profileName); chip.className = 'chip'; }

  for (const b of row.querySelectorAll('.tray button')) setDisabled(b, !!d.busy);
}

// trayButton opens or closes one drive. It is disabled while a job has the
// drive, because a tray opening mid-rip is how a disc gets scratched.
function trayButton(d, action, label) {
  const b = text('button', label);
  b.type = 'button';
  b.dataset.act = action;
  b.title = t(action === 'eject' ? 'drives.openWhy' : 'drives.closeWhy');
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
  if (!d.disc || !d.disc.present) return d.disc && d.disc.error ? d.disc.error : t('disc.none');
  const bits = [];
  if (d.disc.statusName) bits.push(serverWord('discStatus', d.disc.statusName));
  if (d.disc.sectors) bits.push(bytes(d.disc.dataBytes));
  if (d.disc.audioTracks) bits.push(tn('disc.audioTracks', d.disc.audioTracks));
  if (d.disc.dataTracks) bits.push(t('disc.data'));
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
  loadStoredScan();
  if (state.tab === 'files') loadDir(state.path);
}

/* ---------- the workspace ---------- */

// The panes, in the order they appear. Which of them are offered depends on
// what is in the drive: a disc with no filesystem has nothing to browse, and
// a machine that cannot burn is not asked about burning. Hiding what cannot
// be done is most of what makes this page quiet.
const panes = {
  rip:   { id: 'paneRip',   label: 'pane.rip' },
  files: { id: 'paneFiles', label: 'pane.files' },
  dvd:   { id: 'paneDvd',   label: 'pane.dvd' },
  audio: { id: 'paneAudio', label: 'pane.audio' },
  check: { id: 'paneCheck', label: 'pane.check' },
  burn:  { id: 'paneBurn',  label: 'pane.burn' },
  add:   { id: 'paneAdd',   label: 'pane.add' },
  drive: { id: 'paneDrive', label: 'pane.drive' },
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
    // A DVD's films are titles in its index, not the VOBs in its folders,
    // so they get a list of their own rather than a play button on a file
    // that holds three of them.
    if (d.dvdTitles > 0) out.push('dvd');
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
  if (tab === 'dvd' && state.titles.length === 0) loadTitles();
}

function renderSeg(d) {
  const items = segItems(d);
  sync(el('seg'), items, (k) => k, (k) => {
    const b = text('button', t(panes[k].label));
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
    case 'dvd': renderDvdTab(d); break;
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
      ? ((vol && vol.volumeId) || (disc.profileName
          ? t('disc.aProfile', { profile: disc.profileName }) : t('disc.aDisc')))
      : t('disc.nothingIn'));
    // An empty drive says so once, in the heading. The line under it is for
    // what to do about it - or for a fault, which is the one case where the
    // drive has something to say that the heading does not.
    setText(el('discLine'), present ? discSummary(d) : (d.error || t('disc.closeTrayHint')));

    const tags = [];
    if (present) {
      if (disc.profileName) tags.push(['chip', disc.profileName]);
      if (vol && vol.format) tags.push(['chip', vol.format]);
      if (disc.statusName) tags.push(['chip none', serverWord('discStatus', disc.statusName)]);
    }
    sync(el('discTags'), tags, (tag) => tag[1],
      () => text('span', '', 'chip'),
      (node, tag) => { setText(node, tag[1]); node.className = tag[0]; });

    setHidden(el('discMore'), !present);
    if (present) renderDiscFacts(d);
  });
}

function discSummary(d) {
  const disc = d.disc;
  const bits = [];
  if (disc.dataTracks && disc.dataBytes) bits.push(t('disc.ofData', { size: bytes(disc.dataBytes) }));
  if (disc.audioTracks) bits.push(tn('disc.audioTracks', disc.audioTracks));
  if (disc.sessions > 1) bits.push(tn('disc.sessions', disc.sessions));
  if (d.volume && d.volume.format) bits.push(t('disc.readAs', { format: d.volume.format }));
  if (!bits.length) {
    bits.push(serverWord('discStatus', disc.statusName) || t('disc.nothingReadable'));
  }
  return bits.join(' · ');
}

function renderDiscFacts(d) {
  const disc = d.disc;
  const vol = d.volume;
  dl(el('discFacts'), [
    [t('fact.disc'), disc.profileName],
    [t('fact.state'), serverWord('discStatus', disc.statusName) +
      (disc.erasable ? t('fact.erasable') : '')],
    [t('fact.sessions'), disc.sessions || null],
    [t('fact.tracks'), `${disc.tracks ? disc.tracks.length : 0}` +
      (disc.audioTracks ? ` ${t('fact.ofWhichAudio', { n: disc.audioTracks })}` : '')],
    [t('fact.sectors'), disc.sectors ? disc.sectors.toLocaleString(i18n.code) : null],
    [t('fact.asIso'), disc.dataTracks ? bytes(disc.dataBytes) : null],
    [t('fact.asImg'), disc.rawReadable ? bytes(disc.rawBytes) : null],
    [t('fact.volume'), vol ? vol.volumeId : null],
    [t('fact.filesystem'), vol ? (vol.format || 'ISO 9660') : null],
    [t('fact.published'), vol && vol.publisher ? vol.publisher : null],
    [t('fact.mastered'), vol && vol.created ? when(vol.created) : null],
    [t('fact.names'), vol ? namingScheme(vol) : null],
    [t('fact.mediaId'), disc.mediaId || null],
  ]);
}

function renderRipPane(d) {
  memo('rip', [d.canRipIso, d.canRipImg, d.canRipAudio, d.busy,
    d.browseError, d.disc && d.disc.present, d.disc && d.disc.profile], () => {
    // The menu offers only what this disc and this drive can actually do,
    // so nothing here fails after it is pressed.
    const kinds = [];
    if (d.canRipIso) kinds.push(['iso', t('rip.kindIso')]);
    if (d.canRipImg) kinds.push(['img', t('rip.kindImg')]);
    if (d.canRipAudio) kinds.push(['audio', t('rip.kindAudio')]);
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
    return d.browseError || t('rip.nothingReadable');
  }
  const bits = [];
  if (!d.canRipImg && d.disc && d.disc.profile && d.disc.present) {
    bits.push(t('rip.noRawHint'));
  }
  bits.push(t('rip.badSectorHint'));
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
    tags.push(['pack', t('image.unpacked')]);
    if (f.needs) tags.push(['disc', t('image.needs', { disc: f.needs })]);
    return tags;
  }
  const boot = f.boot;
  if (boot && boot.bootable) {
    for (const p of boot.platforms || []) tags.push(['fw', p.name]);
    for (const a of boot.architectures || []) tags.push(['arch', a]);
  } else if (f.error) {
    tags.push(['bad', t('image.notAnISO')]);
  } else if (boot) {
    tags.push(['bad', t('image.willNotBoot')]);
  }
  if (f.needs === noDiscFits) tags.push(['bad', t('image.tooBig')]);
  else if (f.needs) tags.push(['disc', t('image.needs', { disc: f.needs })]);
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
    name.textContent = t(pickable().length ? 'burn.choose' : 'burn.nothingToBurn');
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
    empty.textContent = t(pickable().length ? 'burn.noMatch' : 'burn.noneWhole');
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
    setText(el('burnBlocked'), d.burnBlocker || t('burn.blocked'));
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
    ? t(el('burnSource').value === 'isos' ? 'burn.hintNoneInLibrary' : 'burn.hintNoneInStore')
    : t('burn.hintChecks');
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
    note.textContent = t('burn.archiveNote');
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

// What the Check panel shows. A scan this server still has in memory is
// preferred, because it appears the moment the scan ends and it keeps
// ticking while one runs.
//
// Everything else comes out of the database, by the disc's fingerprint. The
// jobs in memory are lost when the process stops and dropped once sixty
// newer ones exist, and a check that vanished on a restart - or on a reload
// in a browser that had never seen the job - looked exactly like a disc
// nobody had ever checked. The database has had the answer all along; this
// is what asks it.
function latestScan(driveID) {
  return state.jobs.find((j) => j.kind === 'scan' && j.drive === driveID && j.scan) || null;
}

function scanToShow(d) {
  return latestScan(d.id) || state.storedScans.get(d.fingerprint || '') || null;
}

// loadStoredScan fetches the last recorded check of whatever is in the
// selected drive and shapes it like a job, so one function draws both. It is
// asked for once per disc: the answer only changes when a new scan finishes,
// and that arrives as a job instead.
async function loadStoredScan() {
  const d = state.detail;
  const fp = d && d.fingerprint;
  if (!fp || state.storedScans.has(fp)) return;
  // Recorded as attempted before the request goes out, so a disc with no
  // history is not asked about again on every redraw.
  state.storedScans.set(fp, null);
  let r;
  try {
    r = await api(`/api/discs?disc=${encodeURIComponent(fp)}`);
  } catch (e) {
    return; // no history, or none for this disc: the panel simply offers a check
  }
  const disc = (r.discs || [])[0];
  const scans = (disc && disc.scans) || [];
  const last = scans[scans.length - 1];
  if (!last || !last.result) return;
  state.storedScans.set(fp, {
    id: `stored:${last.jobId}`,
    kind: 'scan',
    state: 'done',
    started: last.scannedAt,
    done: 0,
    total: 0,
    scan: last.result,
    fromHistory: true,
  });
  if (state.detail && state.tab === 'check') {
    forget('scan');
    renderScanPanel(state.detail);
  }
}

function gradeWord(grade) {
  const known = ['pristine', 'good', 'worn', 'degraded', 'failing', 'unknown'];
  return known.includes(grade) ? t(`grade.${grade}`) : grade;
}

function gradeClass(grade) {
  if (grade === 'failing' || grade === 'degraded') return 'grade bad';
  if (grade === 'worn') return 'grade warn';
  return 'grade';
}

function renderScanPanel(d) {
  const running = state.jobs.some((j) => j.kind === 'scan' && j.drive === d.id && j.state === 'running');
  setDisabled(el('btnScan'), !!d.busy || !d.disc || !d.disc.present);
  setText(el('scanNote'), t(running ? 'check.running' : 'check.what'));

  const job = scanToShow(d);
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
    head.appendChild(text('span', t('check.checking'), 'grade'));
    const pctDone = job.total > 0 ? Math.round((job.done / job.total) * 100) : 0;
    head.appendChild(text('span', `${pctDone}%`, 'mono muted'));
    const left = eta(job.etaSeconds);
    if (left) head.appendChild(text('span', left, 'muted'));
  } else {
    head.appendChild(text('span', gradeWord(r.grade), gradeClass(r.grade)));
    if (r.score > 0) head.appendChild(text('span', t('check.score', { score: r.score }), 'mono muted'));
    head.appendChild(text('span', t('check.checkedOn', { when: when(job.started) }), 'muted'));
    if (job.fromHistory) head.appendChild(text('span', t('check.fromHistory'), 'muted'));
  }
  box.appendChild(head);

  if (!live) {
    box.appendChild(text('p', r.summary, '')).style.margin = '0 0 10px';
  } else {
    const p = text('p', t(r.c2Supported ? 'check.liveC2' : 'check.liveNoC2'), 'muted');
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
    ? [['s0', 'key.clean'], ['s1', 'key.few'], ['s2', 'key.many'],
       ['s3', 'key.heavy'], ['ss', 'key.struggled'], ['sx', 'key.unreadable']]
    : [['s0', 'key.readCleanly'], ['ss', 'key.struggled'], ['sx', 'key.unreadable']];
  if (live) legend.push(['sp', 'key.notYet']);
  for (const [cls, label] of legend) {
    const sp = document.createElement('span');
    // The swatch takes the strip's own class, so one CSS rule paints both
    // and they cannot drift apart; its size comes from .stripkey i.
    const sw = document.createElement('i');
    sw.className = cls;
    sp.appendChild(sw);
    sp.appendChild(text('span', t(label)));
    key.appendChild(sp);
  }
  key.appendChild(text('span', t('key.direction')));
  box.appendChild(key);

  const facts = document.createElement('dl');
  facts.style.marginTop = '12px';
  dl(facts, [
    [t('check.sectors'), r.sectors ? r.sectors.toLocaleString(i18n.code) : null],
    [t('check.readSoFar'), live && job.total > 0
      ? t('check.percentOfDisc', { pct: Math.round((job.done / job.total) * 100) }) : null],
    [t('check.withErrors'), r.c2Supported
      ? `${r.c2Sectors.toLocaleString(i18n.code)} (${pct(r.c2Sectors, r.sectors)})` : null],
    [t('check.worstSector'), r.c2Supported && r.c2Max ? t('check.ofBytes', { n: r.c2Max }) : null],
    [t('check.unreadable'), r.unreadable
      ? r.unreadable.toLocaleString(i18n.code) : (r.unreadable === 0 ? t('check.noneUnreadable') : null)],
    [t('check.where'), (r.badRanges || []).length ? r.badRanges.slice(0, 6).join(', ') : null],
    [t('check.readAt'), !live && r.avgKbps
      ? t('check.readRate', { avg: Math.round(r.avgKbps), min: Math.round(r.minKbps) }) : null],
    [t('check.struggled'), !live && r.slowStretches
      ? t('check.struggledValue', {
          stretches: tn('check.stretches', r.slowStretches),
          blocks: tn('check.blocks', r.slowBlocks),
        }) : null],
    [t('check.took'), !live && r.readSeconds ? duration(r.readSeconds) : null],
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
    ? t('check.stripPartial', { pct: Math.round((read / map.length) * 100) })
    : t('check.stripWhole'));
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
    el('discsNote').textContent = r.enabled ? t('discs.note') : '';
    renderDiscs();
  } catch (e) { /* the history is not worth an error banner over */ }
}

function renderDiscs() {
  const box = el('discs');
  box.textContent = '';
  setText(el('discsSummary'), tn('discs.count', state.discs.length));
  for (const d of state.discs) {
    const row = document.createElement('div');
    row.className = 'job';

    const head = document.createElement('div');
    head.className = 'top';
    head.appendChild(text('span', d.label || d.disc, 'what'));
    head.appendChild(text('span', gradeWord(d.latestGrade), gradeClass(d.latestGrade)));
    head.appendChild(text('span', tn('discs.checks', d.scans.length), 'muted'));
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
  el('archiveName').placeholder = purpose.suggest || t('archive.takenFromDisc');
  el('archiveNameNote').textContent = purpose.nameNote || t('archive.nameNote');
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
    body.appendChild(text('span',
      `${tOr(`format.${f.id}.name`, f.name)} (${f.extension})`, 'name'));
    body.appendChild(text('span', tOr(`format.${f.id}.note`, f.note), 'why'));
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
  ['browser', 'archive.toBrowser', 'archive.toBrowserWhy'],
  ['store', 'archive.toStore', 'archive.toStoreWhy'],
];

function renderWhere(initial) {
  const box = el('archiveWhere');
  box.textContent = '';
  for (const [id, nameKey, whyKey] of destinations) {
    const name = t(nameKey);
    const why = t(whyKey);
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
        ? t('common.save') : (state.archiveFor ? state.archiveFor.action : t('common.download'));
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
    setText(el('appendBlocked'), d.appendBlocker || t('add.blocked'));
    return;
  }

  const matched = matching(state.images, el('appendFilter').value);
  const shown = matched.slice(0, imagePage);
  setText(el('appendCount'), countLine(shown.length, matched.length, state.images.length));
  sync(el('appendRows'), shown, (f) => f.name, (f) => {
    const tr = document.createElement('tr');
    const tick = document.createElement('td');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.setAttribute('aria-label', t('add.tick', { name: f.name }));
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
    ? t('add.roomEmpty', { free: bytes(free) })
    : t(fits ? 'add.roomFits' : 'add.roomTooBig', { want: bytes(want), free: bytes(free) });
}

/* ---------- files ---------- */

function renderFilesTab(d) {
  const can = d.canBrowse && d.disc && d.disc.present;
  el('filesNone').hidden = can;
  el('filesBody').hidden = !can;
  if (!can) {
    el('filesNone').textContent = d.browseError || t('files.noFilesystem');
  }
}

async function loadDir(path) {
  const id = state.selected;
  if (!id) return;
  try {
    const r = await api(`/api/drives/${encodeURIComponent(id)}/browse?path=${encodeURIComponent(path)}`);
    state.path = r.path;
    state.entries = r.entries;
    state.browse = r;
    fail('');
    renderFiles(r);
  } catch (e) {
    state.entries = [];
    state.browse = null;
    el('fileRows').textContent = '';
    fail(String(e.message || e));
  }
}

// measureDirs asks what is under one or more folders. It is a button rather
// than part of the listing because the answer costs a walk of the whole
// subtree - every directory record in it, one seek each - and doing that for
// every row of every listing would make browsing a disc unusable. Once
// asked, the answer is kept for as long as the disc is in the drive.
async function measureDirs(paths) {
  const id = state.selected;
  const wanted = paths.filter((p) => !state.dirSizes.has(p));
  if (!id || wanted.length === 0) return;
  const btn = el('btnSizes');
  setDisabled(btn, true);
  setText(btn, t('files.measuring'));
  try {
    const query = wanted.map((p) => `path=${encodeURIComponent(p)}`).join('&');
    const r = await api(`/api/drives/${encodeURIComponent(id)}/sizes?${query}`);
    for (const size of r.sizes || []) state.dirSizes.set(size.path, size);
    fail('');
  } catch (e) {
    fail(String(e.message || e));
  }
  setDisabled(btn, false);
  setText(btn, t('files.measure'));
  if (state.browse) renderFiles(state.browse);
}

// folderSizeCell is what goes in the Size column for a directory: the figure
// once it is known, and the offer to work it out until then.
function folderSizeCell(e) {
  const td = document.createElement('td');
  td.className = 'num';
  const known = state.dirSizes.get(e.path);
  if (known && !known.error) {
    td.appendChild(text('span', bytes(known.bytes)));
    td.title = t('files.folderHolds', {
      files: tn('files.fileCount', known.files),
      dirs: tn('files.folderCount', known.dirs),
    });
    return td;
  }
  if (known && known.error) {
    td.appendChild(text('span', '-', 'muted'));
    td.title = known.error;
    return td;
  }
  const b = text('button', t('files.measureOne'), 'link');
  b.type = 'button';
  b.title = t('files.measureWhy');
  b.addEventListener('click', () => measureDirs([e.path]));
  td.appendChild(b);
  return td;
}

function renderFiles(r) {
  const id = state.selected;
  // Breadcrumbs: every level is a way back, which is quicker than Up twice.
  const bc = el('bread');
  bc.textContent = '';
  const parts = r.path === '/' ? [] : r.path.replace(/^\//, '').split('/');
  const root = text('button', r.volume.volumeId || t('files.discRoot'), 'link');
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
  // Nothing to measure in a listing with no folders in it, or one whose
  // folders have all been measured already.
  setDisabled(el('btnSizes'),
    !r.entries.some((e) => e.isDir && !state.dirSizes.has(e.path)));

  const rows = el('fileRows');
  // A player left open in the listing being replaced would go on reading
  // the disc with nothing on screen, so it is stopped rather than dropped.
  for (const open of rows.querySelectorAll('tr[data-player="1"]')) closePlayer(open);
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
  box.setAttribute('aria-label', t('files.tick', { name: e.name }));
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

  tr.appendChild(e.isDir ? folderSizeCell(e) : text('td', bytes(e.size), 'num'));
  tr.appendChild(text('td', when(e.modTime), 'num drop-col'));

  const act = document.createElement('td');
  act.className = 'act';
  if (!e.isDir) {
    const file = `/api/drives/${encodeURIComponent(id)}/file?path=${encodeURIComponent(e.path)}`;
    if (e.playable || e.transcodable) {
      const play = text('button', t('files.play'));
      play.type = 'button';
      // A VOB is a gigabyte of whichever titles happened to land in it, so
      // playing one is not a thing that means anything. The titles are on
      // their own tab, and this is the way to them.
      const partOfDVD = isDVDPart(e.name) && state.detail && state.detail.dvdTitles > 0;
      if (partOfDVD) play.title = t('dvd.playWhy');
      play.addEventListener('click', () => {
        if (partOfDVD) setTab('dvd');
        else togglePlayer(tr, e, file, id);
      });
      act.appendChild(play);
    }
    // One control rather than two. A file can go to this browser or into
    // the image store, and either way it can be called something else on
    // the way out - and that is the same question for a file, a folder and
    // a ticked selection, so it is the same dialog.
    const take = text('button', t('files.take'));
    take.type = 'button';
    take.title = t('files.takeWhy');
    take.addEventListener('click', () => openArchiveDialog({
      title: t('files.takeFileTitle'),
      what: `${e.path} \u2014 ${bytes(e.size)}.`,
      action: t('common.download'),
      suggest: e.name,
      noFormat: true,
      nameNote: t('files.keepsItsName'),
      run: (format, where, name) => where === 'store'
        ? startRip('files', { paths: [e.path], name })
        : downloadDiscFile(id, e.path, name),
    }));
    act.appendChild(take);
  } else {
    const b = text('button', t('files.download'));
    b.type = 'button';
    b.addEventListener('click', () => openArchiveDialog({
      title: t('files.takeFolderTitle'),
      what: t('files.takeFolderWhat', { path: e.path }),
      action: t('common.download'),
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
//
// Two players come out of this. What the browser decodes itself is played
// straight off the disc, byte ranges and all, which is the cheapest thing
// this server can do. Everything else - a DVD's MPEG-2 above all, which no
// browser has played this century - is converted as it is watched and
// arrives as a playlist of segments. That is the only arrangement in which
// the scrub bar works on a film nobody has finished encoding: a jump to the
// middle asks for the segment in the middle and gets it.
function isDVDPart(name) {
  return /^VTS_\d\d_\d\.VOB$/i.test(name);
}

function togglePlayer(tr, e, url, id) {
  const open = tr.nextSibling && tr.nextSibling.dataset && tr.nextSibling.dataset.player === '1';
  if (open) {
    closePlayer(tr.nextSibling);
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
  if (video) { media.style.width = '100%'; media.style.maxWidth = '640px'; }
  cell.appendChild(media);
  const note = text('p', `${e.type} · ${t('files.streamNote')}`, 'muted');
  note.style.fontSize = '11px';
  note.style.margin = '6px 0 0';
  cell.appendChild(note);
  row.appendChild(cell);
  tr.after(row);

  if (e.playable) {
    media.src = url + '&inline=1';
    // A browser can be wrong about what it plays: the container is one it
    // knows and the codec inside it is not. There is no way to find that
    // out except by trying, so the failure is caught here and turned into
    // the converted stream rather than into a player that sits there.
    media.addEventListener('error', () => {
      if (e.transcodable) {
        setText(note, t('files.converting'));
        startConverted(row, media, note, id, e);
      } else {
        playerFailed(note, t('files.cannotPlay'));
      }
    });
    media.play().catch(() => { /* the browser would rather the user pressed play */ });
    return;
  }
  setText(note, t('files.converting'));
  startConverted(row, media, note, id, e);
}

// closePlayer puts the player away and, more to the point, stops it: a
// converted stream that is left running keeps the drive reading a film
// nobody is watching any more.
function closePlayer(row) {
  if (row.hlsPlayer) {
    row.hlsPlayer.destroy();
    row.hlsPlayer = null;
  }
  const media = row.querySelector('video, audio');
  if (media) {
    media.pause();
    media.removeAttribute('src');
    media.load();
  }
  row.remove();
}

// startConverted points the player at the playlist this server encodes on
// demand. Safari plays such a playlist itself; everything else needs the
// library, which is fetched only now - a page that never plays a DVD never
// loads it.
function startConverted(row, media, note, id, e) {
  const what = e.title
    ? `title=${encodeURIComponent(e.title)}`
    : `path=${encodeURIComponent(e.path)}`;
  const src = `/api/drives/${encodeURIComponent(id)}/hls/index.m3u8?${what}`;
  // The library first, and the browser's own playlist support only where
  // there is no library to use.
  //
  // It was the other way round, and that is a trap. Chromium answers
  // canPlayType('application/vnd.apple.mpegurl') with "maybe" and then does
  // not play one: it reads the playlist, shows the right length, buffers
  // six seconds and stops with "Parsed buffers not in DTS sequence". A
  // player showing the length of a film it will never start is exactly the
  // failure this whole path was built to end, so the question is not asked
  // of a browser that has a working answer of its own.
  loadHls().then((Hls) => {
    if (!Hls || !Hls.isSupported()) {
      playNativeHLS(media, note, src);
      return;
    }
    // A segment does not exist until the disc has been read and ffmpeg has
    // finished with it, which on an optical drive is seconds rather than
    // milliseconds. The defaults here are tuned for a CDN and give up long
    // before that, and giving up means asking again, which is how a player
    // can wait for ever for a segment nobody ever finishes.
    const hls = new Hls({
      enableWorker: true,
      manifestLoadingTimeOut: 60000,
      manifestLoadingMaxRetry: 2,
      fragLoadingTimeOut: 180000,
      fragLoadingMaxRetry: 2,
      // Without this the player abandons a fragment it decides is arriving
      // too slowly - which is every fragment, when each one is being made
      // to order.
      abrEwmaDefaultEstimate: 5000000,
      testBandwidth: false,
    });
    row.hlsPlayer = hls;
    hls.on(Hls.Events.ERROR, (_evt, data) => {
      if (!data || !data.fatal) return;
      hls.destroy();
      row.hlsPlayer = null;
      playerFailed(note, t('files.convertFailed'));
    });
    hls.loadSource(src);
    hls.attachMedia(media);
    media.play().catch(() => { /* the browser would rather the user pressed play */ });
  }).catch(() => playNativeHLS(media, note, src));
}

// playNativeHLS is the Safari path, and the last resort anywhere else: a
// browser that plays a playlist by itself needs nothing but the URL.
function playNativeHLS(media, note, src) {
  if (!media.canPlayType || !media.canPlayType('application/vnd.apple.mpegurl')) {
    playerFailed(note, t('files.cannotPlay'));
    return;
  }
  media.src = src;
  media.play().catch(() => { /* the browser would rather the user pressed play */ });
}

// playerFailed says why nothing is happening. A silent player that never
// loads is the bug this whole path exists to end, so every way out of it
// ends here.
function playerFailed(note, why) {
  setText(note, `${why} ${t('files.tryVLC')}`);
  note.classList.add('why');
}

// loadHls fetches the player library once, from this server rather than
// from anywhere else: ripperX runs on machines that are not on the
// internet, and a page that needs a CDN would not work on them.
function loadHls() {
  if (window.Hls) return Promise.resolve(window.Hls);
  if (!loadHls.pending) {
    loadHls.pending = new Promise((resolve, reject) => {
      const tag = document.createElement('script');
      tag.src = 'vendor/hls.light.min.js';
      tag.addEventListener('load', () => resolve(window.Hls));
      tag.addEventListener('error', () => {
        loadHls.pending = null;
        reject(new Error('the player library could not be loaded'));
      });
      document.head.appendChild(tag);
    });
  }
  return loadHls.pending;
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
  fail(t('files.oneAtATime'));
}

function downloadDiscFile(id, path, name) {
  window.location.href =
    `/api/drives/${encodeURIComponent(id)}/file?path=${encodeURIComponent(path)}${nameParam(name)}`;
}

function updatePicked() {
  const n = state.picked.size;
  el('btnRipSel').disabled = n === 0;
  el('selNote').textContent = n === 0 ? '' : tn('files.selected', n);
}

/* ---------- DVD titles ---------- */

async function loadTitles() {
  const id = state.selected;
  if (!id) return;
  try {
    const r = await api(`/api/drives/${encodeURIComponent(id)}/titles`);
    state.titles = r.titles || [];
    fail('');
  } catch (e) {
    state.titles = [];
    fail(String(e.message || e));
  }
  if (state.detail) renderPane(state.detail);
}

function renderDvdTab(d) {
  // Rebuilding this would tear down a player in mid-sentence, which is what
  // a snapshot twice a second would otherwise do for as long as the tab is
  // open.
  memo('dvd', [d.id, state.titles.map((x) => `${x.number}:${x.seconds}`).join('|')],
    () => drawDvdTab(d));
}

function drawDvdTab(d) {
  const titles = state.titles;
  setHidden(el('dvdNone'), titles.length > 0);
  setHidden(el('dvdBody'), titles.length === 0);
  if (titles.length === 0) {
    setText(el('dvdNone'), t('dvd.none'));
    return;
  }
  const id = d.id;
  const rows = el('titleRows');
  for (const open of rows.querySelectorAll('tr[data-player="1"]')) closePlayer(open);
  rows.textContent = '';
  for (const title of titles) {
    rows.appendChild(titleRow(id, title));
  }
}

function titleRow(id, title) {
  const tr = document.createElement('tr');
  tr.appendChild(text('td', t('dvd.titleNumber', { n: String(title.number).padStart(2, '0') })));
  tr.appendChild(text('td', clock(title.seconds), 'num'));
  tr.appendChild(text('td', bytes(title.bytes), 'num drop-col'));

  const act = document.createElement('td');
  act.className = 'act';
  // A title is played the same way a file no browser decodes is: converted
  // as it is watched. What is different is that the disc knows how long it
  // is, so the scrub bar is right from the first frame.
  const play = text('button', t('files.play'));
  play.type = 'button';
  play.addEventListener('click', () => togglePlayer(tr, {
    name: t('dvd.titleNumber', { n: String(title.number).padStart(2, '0') }),
    type: 'video/mpeg', playable: false, transcodable: true, title: title.number,
  }, '', id));
  act.appendChild(play);

  const take = text('button', t('files.take'));
  take.type = 'button';
  take.addEventListener('click', () => {
    window.location.href = `/api/drives/${encodeURIComponent(id)}/file?title=${title.number}`;
  });
  act.appendChild(take);
  tr.appendChild(act);
  return tr;
}

// clock is a length as somebody reads it off a disc sleeve.
function clock(seconds) {
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const rest = String(s % 60).padStart(2, '0');
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${rest}` : `${m}:${rest}`;
}

/* ---------- audio ---------- */

function renderAudioTab(d) {
  const tracks = (d.disc && d.disc.tracks || []).filter((tr) => tr.audio);
  // Rebuilding this rebuilt the <audio> elements, which stopped whatever was
  // playing and lost its position - twice a second, for as long as the tab
  // was open.
  memo('audio', [d.id, d.canRipAudio, d.disc && d.disc.present,
    tracks.map((x) => `${x.number}:${x.durationSeconds}`).join('|')],
    () => drawAudioTab(d, tracks));
}

function drawAudioTab(d, tracks) {
  const can = tracks.length > 0;
  setHidden(el('audioNone'), can);
  setHidden(el('audioBody'), !can);
  if (!can) {
    setText(el('audioNone'), t(d.disc && d.disc.present ? 'audio.noTracks' : 'audio.noDisc'));
    return;
  }
  const id = d.id;
  el('btnM3U').href = `/api/drives/${encodeURIComponent(id)}/playlist.m3u`;
  el('btnRipAudio').disabled = !d.canRipAudio;

  const rows = el('trackRows');
  rows.textContent = '';
  for (const track of tracks) {
    const tr = document.createElement('tr');
    tr.appendChild(text('td', t('audio.trackNumber', { n: String(track.number).padStart(2, '0') }) +
      (track.preEmphasis ? ` ${t('audio.preEmphasis')}` : '')));
    tr.appendChild(text('td', duration(track.durationSeconds), 'num'));

    const play = document.createElement('td');
    const audio = document.createElement('audio');
    audio.controls = true;
    audio.preload = 'none';
    audio.src = `/api/drives/${encodeURIComponent(id)}/audio/${track.number}.wav`;
    play.appendChild(audio);
    tr.appendChild(play);

    const act = document.createElement('td');
    act.className = 'act';
    const a = document.createElement('a');
    a.className = 'btn';
    a.href = audio.src;
    a.textContent = t('audio.downloadWav');
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
    el('capNotes').textContent = d.error || t('drive.silent');
    return;
  }
  const x = (kb) => (kb ? `${(kb / 176).toFixed(0)}x (${kb} ${t('unit.kbps')})` : null);
  dl(el('driveFacts'), [
    [t('drive.model'), `${c.info.vendor} ${c.info.product}`],
    [t('drive.firmware'), c.info.version],
    [t('drive.serial'), c.serialNumber || null],
    [t('drive.loading'), serverWord('mech', c.loadingMechanism)],
    [t('drive.buffer'), c.bufferKb ? `${c.bufferKb} ${t('unit.kb')}` : null],
    [t('drive.readsUpTo'), x(c.maxReadSpeedKb)],
    [t('drive.readingAt'), x(c.currentReadSpeedKb)],
    [t('drive.writesUpTo'), x(c.maxWriteSpeedKb)],
    [t('drive.inTheDrive'), c.currentProfileName],
  ]);

  // Each line carries the name of the ability as well as the sentence, so
  // it can be translated by the name and fall back to the sentence the
  // server sent when a locale has no words for it yet.
  const fill = (node, list, can) => {
    node.textContent = '';
    for (const a of list || []) {
      node.appendChild(text('li', tOr(`cap.${a.name}.${can ? 'can' : 'cannot'}`, a.text)));
    }
  };
  fill(el('can'), c.can, true);
  fill(el('cannot'), c.cannot, false);
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
  setText(el('doneSummary'), tn('jobs.finished', done.length));
  sync(el('doneJobs'), done, (j) => j.id, createJob, updateJob);
}

// jobState is the one word a job's state is shown as. The server's own
// vocabulary is the key rather than the text, so a page in another language
// does not show four English words among its own.
function jobState(state) {
  const known = ['running', 'done', 'failed', 'cancelled'];
  return known.includes(state) ? t(`jobs.state.${state}`) : state;
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
  const stop = text('button', t('jobs.stop'));
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
  setText(div.querySelector('.pct'), j.total > 0 ? `${Math.round(share)}%` : jobState(j.state));

  const bits = [];
  if (j.phase) bits.push(j.phase);
  if (j.total > 0) bits.push(t('jobs.ofTotal', { done: bytes(j.done), total: bytes(j.total) }));
  if (j.bytesPerSec > 0) bits.push(rate(j.bytesPerSec));
  if (running) {
    bits.push(t('jobs.soFar', { elapsed: since(j.started) }));
    const left = eta(j.etaSeconds);
    if (left) bits.push(left);
  } else {
    bits.push(`${jobState(j.state)} · ${when(j.started)}`);
  }
  if (j.badSectors > 0) {
    bits.push(tn('jobs.unreadable', j.badSectors) +
      (j.badRanges && j.badRanges.length
        ? ' ' + t('jobs.atRanges', { ranges: j.badRanges.slice(0, 4).join(', ') }) : ''));
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

  sync(div.querySelector('.targets'), j.targets || [], (name) => name, (name) => {
    const a = document.createElement('a');
    a.className = 'btn';
    a.href = `/api/images/${encodeURIComponent(name)}`;
    a.textContent = name;
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
  if (vol.format === 'UDF') return t('names.udf');
  if (vol.joliet) return 'Joliet';
  if (vol.rockRidge) return 'Rock Ridge';
  return t('names.isoOnly');
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
    setText(el('storeLine'), r.store);
    renderSpace(r);
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
// page is drawn until the rest are asked for.
//
// One screenful, deliberately. The panel scrolls, and a page that is longer
// than the box it sits in means scrolling to find out there is more below -
// which is the thing the filter and the count exist to avoid.
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
function countLine(shown, matched, total) {
  if (total === 0) return '';
  if (matched < total) {
    return shown < matched
      ? t('count.shownOfMatching', { shown, matched, total })
      : t('count.matchingOfTotal', { matched, total });
  }
  return shown < total
    ? t('count.shownOfTotal', { shown, total })
    : tn('count.total', total);
}

// A dual-layer rip is 8 GB and a share fills as quietly as a disk does. The
// bar turns when there is less than one disc's worth left, because that is
// the point at which the next rip is the one that fails.
const lowSpace = 9 << 30;

function renderSpace(r) {
  const box = el('storeSpace');
  setHidden(box, !r.total);
  if (!r.total) return;
  const used = r.total - (r.free || 0);
  const share = Math.max(0, Math.min(100, (used / r.total) * 100));
  const low = (r.free || 0) < lowSpace;
  const meter = box.querySelector('.meter');
  setWidth(meter.firstChild, `${share.toFixed(1)}%`);
  setClass(meter, 'low', low);
  setClass(el('storeFree'), 'low', low);
  setText(el('storeFree'), t('images.freeOf', { free: bytes(r.free), total: bytes(r.total) }) +
    (low ? ` \u2014 ${t('images.lowSpace')}` : ''));
}

function renderImages() {
  const rows = el('imageRows');
  rows.textContent = '';
  el('noImages').hidden = state.images.length > 0;

  const matched = matching(state.images, el('imageFilter').value);
  const shown = state.imagesAll ? matched : matched.slice(0, imagePage);
  setText(el('imageCount'), countLine(shown.length, matched.length, state.images.length));
  setHidden(el('imageMore'), state.imagesAll || matched.length <= shown.length);
  setText(el('imageMore'), t('images.showOther', { n: matched.length - shown.length }));

  for (const f of shown) {
    const tr = document.createElement('tr');
    tr.appendChild(text('td', f.name, 'wrapname'));
    tr.appendChild(text('td', bytes(f.size), 'num'));

    const act = document.createElement('td');
    act.className = 'act';

    const a = document.createElement('a');
    a.className = 'btn';
    a.href = `/api/images/${encodeURIComponent(f.name)}`;
    a.textContent = t('common.download');
    act.appendChild(a);

    // A raw image can be turned into something burnable without going back
    // to the disc, which is the only reason this button is here.
    if (f.size % 2352 === 0 && f.size % 2048 !== 0) {
      const conv = text('button', t('images.toIso'));
      conv.type = 'button';
      conv.addEventListener('click', async () => {
        conv.disabled = true;
        try { await post('/api/convert', { name: f.name }); }
        catch (e) { fail(String(e.message || e)); conv.disabled = false; }
      });
      act.appendChild(conv);
    }

    const del = text('button', t('common.delete'));
    del.type = 'button';
    del.addEventListener('click', async () => {
      const yes = await ask({
        title: t('images.deleteTitle'),
        what: `${f.name} \u2014 ${bytes(f.size)}`,
        action: t('images.deleteGo'),
        danger: t('images.deleteWarn'),
      });
      if (!yes) return;
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
  el('upNote').textContent = t('upload.uploading', { name: file.name });

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
      el('upNote').textContent = (body && body.error) || t('upload.failedStatus', { status: xhr.status });
      return;
    }
    el('upNote').textContent = t('upload.stored', { name: body.file.name, sum: body.sha256 });
    await loadImages();
  });
  xhr.addEventListener('error', () => {
    bar.hidden = true;
    el('upNote').textContent = t('upload.unfinished');
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
    // The name box is emptied once it has been used. A name that was typed
    // is used exactly as it is, which means no date is added to it - so a
    // name left sitting in the box silently became the name of every later
    // rip too, and the dates that keep two rips of one disc apart stopped
    // appearing. Emptying it puts the next rip back on the disc's own name
    // and today's date unless somebody says otherwise again.
    if (!extra || extra.name === undefined) el('ripName').value = '';
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
      title: t('files.takeTickedTitle'),
      what: tn('files.takeTickedWhat', n),
      action: t('common.save'),
      where: 'store',
      suggest: t('archive.fromDiscLabel'),
      run: (format, where, name) => where === 'store'
        ? startRip('files', { paths: Array.from(state.picked), format, name })
        : downloadPicked(name),
    });
  });

  el('btnSizes').addEventListener('click', () =>
    measureDirs(state.entries.filter((e) => e.isDir).map((e) => e.path)));

  el('btnArchive').addEventListener('click', () => openArchiveDialog({
    title: t('files.takeFolderTitle'),
    what: t('files.takeFolderWhat', { path: state.path }),
    action: t('common.download'),
    suggest: state.path === '/' ? t('archive.fromDiscLabel') : state.path.split('/').pop(),
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
    let title = t('burn.confirmTitle');
    let danger = t('burn.confirmWarn');
    let what = t('burn.confirmWhat', { image, drive: d.id });
    if (unpack) {
      title = t('burn.unpackTitle');
      what = t('burn.unpackWhat', { image, drive: d.id });
      danger = t('burn.unpackWarn');
    } else if (dummy) {
      title = t('burn.rehearseTitle');
      danger = '';
      what = t('burn.rehearseWhat', { image });
    }
    // If it will not boot, say so here rather than after the disc is spent.
    const info = state.burnInfo;
    if (!dummy && info && info.boot && !info.boot.bootable) {
      danger += `\n${upperFirst(info.boot.why || t('burn.willNotBoot'))}`;
    }
    const yes = await ask({
      title, what, danger,
      action: t(dummy ? 'burn.rehearseGo' : 'burn.go2'),
    });
    if (!yes) return;
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
    const yes = await ask({
      title: t('add.confirmTitle'),
      what: tn('add.confirmWhat', names.length, { drive: d.id }),
      action: t('add.confirmGo'),
      danger: closing ? t('add.confirmClose') : '',
    });
    if (!yes) return;
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
    const yes = await ask({
      title: t('erase.title'),
      what: t('erase.what', { drive: d.id }),
      action: t('erase.go'),
      danger: t('erase.warn'),
    });
    if (!yes) return;
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
    state.titles = [];
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
    // A scan that has just finished makes whatever the database said stale.
    state.storedScans.clear();
    loadStoredScan();
  }
}

function connect() {
  const es = new EventSource('/api/events');
  es.addEventListener('open', () => {
    el('conn').className = 'conn live';
    setText(el('conn'), t('conn.live'));
    el('conn').title = t('conn.liveWhy');
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
    setText(el('conn'), t('conn.reconnecting'));
  });
}

// renderLanguages fills the menu from the locale files this build carries,
// so a language added to the build appears here without anything else
// changing. With only one installed there is nothing to choose and the menu
// stays out of the way.
function renderLanguages() {
  const pick = el('lang');
  const langs = i18n.languages;
  setHidden(pick, langs.length < 2);
  if (langs.length < 2) return;
  pick.textContent = '';
  for (const l of langs) {
    const o = document.createElement('option');
    o.value = l.code;
    o.textContent = l.name;
    pick.appendChild(o);
  }
  pick.value = i18n.code;
  pick.addEventListener('change', () => i18n.choose(pick.value));
}

async function main() {
  await i18n.start();
  renderLanguages();
  wireActions();
  try {
    state.status = await api('/api/status');
  } catch (e) {
    fail(String(e.message || e));
    return;
  }
  const s = state.status;
  el('btnLogout').hidden = !s.authOn;
  el('burnerFoot').textContent = s.burner ? `${s.burnerKind} (${s.burner})` : t('foot.noBurnerYet');
  el('status').textContent = [
    `ripperX ${s.version}`,
    tn('app.driveCount', s.drives),
    t('app.imagesIn', { store: s.store }),
    s.allowBurn ? t('app.burningWith', { burner: s.burnerKind }) : t('app.readOnly'),
  ].join(' · ');
  if (!s.allowBurn && s.burner === '') {
    el('warning').textContent = t('app.noBurner');
    el('warning').hidden = false;
  }
  el('storeFoot').textContent = s.store;
  await Promise.all([loadImages(), loadISOs(), loadFormats(), loadDiscs()]);
  connect();
}

main();

