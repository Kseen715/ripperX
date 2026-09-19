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
  tab: 'disc',
  detail: null,      // /api/drives/<id>, which includes the volume and capabilities
  discKey: '',       // identifies the disc; a change means reload everything below
  path: '/',
  entries: [],
  picked: new Set(),
  images: [],
  formats: [],
  discs: [],
  // What the archive dialog is for when it opens: a download of one folder,
  // or a rip of whatever is ticked.
  archiveFor: null,
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

// discKey changes exactly when the disc does. It is what decides whether the
// file listing and the volume details still describe what is in the drive.
function discKey(d) {
  if (!d || !d.disc || !d.disc.present) return 'empty';
  return [d.disc.profile, d.disc.sectors, d.disc.tracks ? d.disc.tracks.length : 0].join('/');
}

function renderDrives() {
  const box = el('drives');
  box.textContent = '';
  el('noDrives').hidden = state.drives.length > 0;

  for (const d of state.drives) {
    const card = document.createElement('div');
    card.className = 'card' + (d.id === state.selected ? ' on' : '');

    // Choosing a drive and opening its tray are different things, so they
    // are different controls: the heading selects, the buttons act.
    const pick = document.createElement('button');
    pick.type = 'button';
    pick.className = 'pick';
    pick.setAttribute('aria-pressed', d.id === state.selected ? 'true' : 'false');
    pick.addEventListener('click', () => select(d.id));

    const name = document.createElement('div');
    name.appendChild(text('h2', d.name || d.id));
    name.appendChild(text('div', d.path, 'dev'));
    pick.appendChild(name);

    if (d.busy) pick.appendChild(text('span', 'working', 'chip busy'));
    else if (!d.disc || !d.disc.present) pick.appendChild(text('span', 'empty', 'chip none'));
    else pick.appendChild(text('span', d.disc.profileName, 'chip'));
    card.appendChild(pick);

    card.appendChild(text('div', describeDisc(d), 'muted'));

    if (state.status && state.status.allowEject) {
      const tray = document.createElement('div');
      tray.className = 'tray';
      tray.appendChild(trayButton(d, 'eject', 'Open'));
      tray.appendChild(trayButton(d, 'load', 'Close'));
      const refresh = text('button', 'Re-read');
      refresh.type = 'button';
      refresh.title = 'Forget what is known about the disc and ask the drive again';
      refresh.disabled = !!d.busy;
      refresh.addEventListener('click', async () => {
        refresh.disabled = true;
        try { await post(`/api/drives/${encodeURIComponent(d.id)}/refresh`); fail(''); }
        catch (e) { fail(String(e.message || e)); }
        if (d.id === state.selected) loadDetail();
      });
      tray.appendChild(refresh);
      card.appendChild(tray);
    }
    box.appendChild(card);
  }

  if (!state.selected && state.drives.length === 1) select(state.drives[0].id);
  if (state.selected && !driveByID(state.selected)) select(null);
}

// trayButton opens or closes one drive. It is disabled while a job has the
// drive, because a tray opening mid-rip is how a disc gets scratched.
function trayButton(d, action, label) {
  const b = text('button', label);
  b.type = 'button';
  b.disabled = !!d.busy;
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

function setTab(tab) {
  state.tab = tab;
  for (const b of document.querySelectorAll('.tabs button')) {
    const on = b.dataset.tab === tab;
    b.classList.toggle('active', on);
    b.setAttribute('aria-pressed', on ? 'true' : 'false');
  }
  el('tabDisc').hidden = tab !== 'disc';
  el('tabFiles').hidden = tab !== 'files';
  el('tabAudio').hidden = tab !== 'audio';
  el('tabDrive').hidden = tab !== 'drive';
  if (tab === 'files' && state.entries.length === 0) loadDir(state.path);
}

function renderWork() {
  const d = state.detail;
  if (!d) return;
  el('workTitle').textContent = `${d.name || d.id} — ${d.path}`;
  renderDiscTab(d);
  renderFilesTab(d);
  renderAudioTab(d);
  renderDriveTab(d);
}

function dl(target, pairs) {
  target.textContent = '';
  for (const [k, v] of pairs) {
    if (v === null || v === undefined || v === '') continue;
    target.appendChild(text('dt', k));
    target.appendChild(text('dd', v));
  }
}

function renderDiscTab(d) {
  const disc = d.disc;
  const empty = !disc || !disc.present;
  el('discNone').hidden = !empty;
  el('discBody').hidden = empty;
  if (empty) {
    el('discNone').textContent = (disc && disc.error) || d.error ||
      'There is no disc in this drive. Put one in with the Close button on its card, or press Re-read.';
    renderScanPanel(d);
    renderBurnBox(d);
    renderAppendBox(d);
    return;
  }

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
    ['Published', vol && vol.publisher ? vol.publisher : null],
    ['Mastered', vol && vol.created ? when(vol.created) : null],
    ['Names', vol ? (vol.joliet ? 'Joliet' : (vol.rockRidge ? 'Rock Ridge' : 'ISO 9660 only')) : null],
    ['Media id', disc.mediaId || null],
  ]);

  // The rip menu offers only what this disc and this drive can actually
  // do, so nothing here fails after it is pressed.
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
  el('btnRip').disabled = kinds.length === 0;
  el('ripLengthField').hidden = sel.value !== 'iso';
  el('ripHint').textContent = ripHint(d);
  renderScanPanel(d);
  renderBurnBox(d);
  renderAppendBox(d);
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

function renderBurnBox(d) {
  const allowed = state.status && state.status.allowBurn;
  el('burnBox').hidden = !allowed;
  if (!allowed) return;
  const blocked = !d.canBurn;
  el('burnBlocked').hidden = !blocked;
  el('burnFields').hidden = blocked;
  if (blocked) {
    el('burnBlocked').textContent = d.burnBlocker || 'This disc cannot be written to.';
  }
  el('btnErase').hidden = !(d.disc && d.disc.present && d.disc.erasable);

  const sel = el('burnImage');
  const was = sel.value;
  sel.textContent = '';
  const burnable = state.images.filter((f) => f.size > 0 && f.size % 2048 === 0);
  for (const f of burnable) {
    const o = document.createElement('option');
    o.value = f.name;
    o.textContent = `${f.name} — ${bytes(f.size)}`;
    sel.appendChild(o);
  }
  if (burnable.some((f) => f.name === was)) sel.value = was;
  el('btnBurn').disabled = burnable.length === 0;
  el('burnHint').textContent = burnable.length === 0
    ? 'No image in the store is a whole number of 2048-byte sectors. Upload an .iso, or convert a raw .img below.'
    : 'Everything that can be checked is checked before the laser is switched on. Afterwards every sector is read back and compared with the image.';
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
  const box = el('scanResult');
  const running = state.jobs.some((j) => j.kind === 'scan' && j.drive === d.id && j.state === 'running');
  el('btnScan').disabled = !!d.busy || !d.disc || !d.disc.present;
  el('scanNote').textContent = running
    ? 'Reading every sector. This takes about as long as ripping the disc.'
    : 'Counts the bytes the drive\u2019s error correction could not fix. A disc reads perfectly right up until it does not \u2014 this is what shows the decline while there is still time to copy it.';

  const job = latestScan(d.id);
  box.hidden = !job;
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
  for (const d of state.discs) {
    const row = document.createElement('div');
    row.className = 'job';

    const head = document.createElement('div');
    head.className = 'head';
    head.appendChild(text('b', d.label || d.disc));
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
  if (state.formats.length === 0) return;
  state.archiveFor = purpose;
  el('archiveTitle').textContent = purpose.title;
  el('archiveWhat').textContent = purpose.what;
  el('archiveGo').textContent = purpose.action;

  const box = el('archiveFormats');
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

// A disc with data on it usually cannot be burned and very often can be
// added to. That is the case this panel exists for: a DVD with a third of a
// gigabyte going spare is not a disc to throw away.
const appendPicked = new Set();

function renderAppendBox(d) {
  const allowed = state.status && state.status.allowBurn;
  el('appendBox').hidden = !allowed;
  if (!allowed) return;

  const blocked = !d.canAppend;
  el('appendBlocked').hidden = !blocked;
  el('appendFields').hidden = blocked;
  if (blocked) {
    el('appendBlocked').textContent = d.appendBlocker || 'Files cannot be added to this disc.';
    return;
  }

  const free = (d.disc && d.disc.writableBytes) || 0;
  const rows = el('appendRows');
  rows.textContent = '';
  for (const f of state.images) {
    const tr = document.createElement('tr');
    const tick = document.createElement('td');
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = appendPicked.has(f.name);
    box.setAttribute('aria-label', `add ${f.name}`);
    box.addEventListener('change', () => {
      if (box.checked) appendPicked.add(f.name); else appendPicked.delete(f.name);
      updateAppend(free);
    });
    tick.appendChild(box);
    tr.appendChild(tick);
    tr.appendChild(text('td', f.name, 'wrapname'));
    tr.appendChild(text('td', bytes(f.size), 'num'));
    rows.appendChild(tr);
  }
  // A file that has been deleted from the store since it was ticked must
  // not stay in the selection and then fail at the far end.
  for (const name of Array.from(appendPicked)) {
    if (!state.images.some((f) => f.name === name)) appendPicked.delete(name);
  }
  updateAppend(free);
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
  tr.appendChild(text('td', when(e.modTime), 'num'));

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
    const a = document.createElement('a');
    a.className = 'btn';
    a.href = file;
    a.textContent = 'Download';
    act.appendChild(a);
  } else {
    const b = text('button', 'Download\u2026');
    b.type = 'button';
    b.addEventListener('click', () => openArchiveDialog({
      title: 'Download folder',
      what: `${e.path} \u2014 everything under it, as one archive.`,
      action: 'Download',
      run: (format) => downloadArchive(id, e.path, format),
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
function downloadArchive(id, path, format) {
  window.location.href =
    `/api/drives/${encodeURIComponent(id)}/archive?path=${encodeURIComponent(path)}&format=${encodeURIComponent(format)}`;
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
  const can = tracks.length > 0;
  el('audioNone').hidden = can;
  el('audioBody').hidden = !can;
  if (!can) {
    el('audioNone').textContent = d.disc && d.disc.present
      ? 'There are no audio tracks on this disc.'
      : 'There is no disc in this drive.';
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

function renderJobs() {
  const box = el('jobs');
  box.textContent = '';
  el('noJobs').hidden = state.jobs.length > 0;
  for (const j of state.jobs) box.appendChild(jobRow(j));
}

function jobRow(j) {
  const div = document.createElement('div');
  div.className = 'job';

  const head = document.createElement('div');
  head.className = 'head';
  head.appendChild(text('b', j.label));
  head.appendChild(text('span', j.state, 'chip' + (j.state === 'running' ? ' busy' : '')));
  if (j.state === 'running') {
    const stop = text('button', 'Stop');
    stop.type = 'button';
    stop.style.marginLeft = 'auto';
    stop.addEventListener('click', async () => {
      stop.disabled = true;
      try { await post(`/api/jobs/${encodeURIComponent(j.id)}/cancel`); }
      catch (e) { fail(String(e.message || e)); }
    });
    head.appendChild(stop);
  }
  div.appendChild(head);

  if (j.total > 0) {
    const bar = document.createElement('div');
    bar.className = 'bar' + (j.state === 'done' ? ' done' : (j.state === 'failed' ? ' bad' : ''));
    const fillBar = document.createElement('i');
    fillBar.style.width = `${Math.min(100, (j.done / j.total) * 100).toFixed(1)}%`;
    bar.appendChild(fillBar);
    bar.style.marginTop = '8px';
    div.appendChild(bar);
  }

  const bits = [];
  if (j.phase) bits.push(j.phase);
  if (j.total > 0) bits.push(`${bytes(j.done)} of ${bytes(j.total)}`);
  if (j.bytesPerSec > 0) bits.push(rate(j.bytesPerSec));
  if (j.state === 'running') {
    bits.push(`${since(j.started)} so far`);
    const left = eta(j.etaSeconds);
    if (left) bits.push(left);
  } else {
    bits.push(when(j.started));
  }
  if (j.badSectors > 0) {
    bits.push(`${j.badSectors} unreadable sector${j.badSectors > 1 ? 's' : ''}` +
      (j.badRanges && j.badRanges.length ? ` at ${j.badRanges.slice(0, 4).join(', ')}` : ''));
  }
  const meta = text('p', bits.join(' · '), 'meta');
  div.appendChild(meta);

  if (j.message) div.appendChild(text('p', j.message, 'meta'));
  if (j.error) {
    const e = text('p', j.error, 'meta');
    e.style.color = 'var(--accent)';
    div.appendChild(e);
  }
  if (j.sha256) div.appendChild(text('p', `SHA-256 ${j.sha256}`, 'meta mono'));

  if (j.targets && j.targets.length) {
    const row = document.createElement('p');
    row.className = 'meta';
    row.style.display = 'flex';
    row.style.flexWrap = 'wrap';
    row.style.gap = '6px';
    for (const t of j.targets) {
      const a = document.createElement('a');
      a.className = 'btn';
      a.style.fontSize = '11px';
      a.style.padding = '3px 8px';
      a.href = `/api/images/${encodeURIComponent(t)}`;
      a.textContent = t;
      row.appendChild(a);
    }
    div.appendChild(row);
  }
  return div;
}

/* ---------- images ---------- */

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

function renderImages() {
  const rows = el('imageRows');
  rows.textContent = '';
  el('noImages').hidden = state.images.length > 0;
  for (const f of state.images) {
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
  for (const b of document.querySelectorAll('.tabs button')) {
    b.addEventListener('click', () => setTab(b.dataset.tab));
  }
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
      title: 'Save to images',
      what: `${n} selected items, wrapped in one archive in the image store.`,
      action: 'Save',
      run: (format) => startRip('files', { paths: Array.from(state.picked), format }),
    });
  });

  el('btnArchive').addEventListener('click', () => openArchiveDialog({
    title: 'Download folder',
    what: `${state.path} \u2014 everything under it, as one archive.`,
    action: 'Download',
    run: (format) => downloadArchive(state.selected, state.path, format),
  }));

  el('archiveDialog').addEventListener('close', () => {
    const dlg = el('archiveDialog');
    const purpose = state.archiveFor;
    state.archiveFor = null;
    if (dlg.returnValue === 'ok' && purpose) purpose.run(chosenFormat());
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

  el('btnBurn').addEventListener('click', async () => {
    const d = state.detail;
    const image = el('burnImage').value;
    if (!d || !image) return;
    const dummy = el('burnDummy').checked;
    const what = dummy
      ? `Rehearse writing ${image} in ${d.id}? The laser stays off and the disc is untouched.`
      : `Write ${image} to the disc in ${d.id}?\n\nThis cannot be undone: whatever is on the disc now is gone.`;
    if (!window.confirm(what)) return;
    try {
      await post('/api/burn', {
        drive: d.id,
        image,
        speedX: Number(el('burnSpeed').value) || 0,
        dummy,
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
    loadDetail();
    return;
  }
  // The shallow view still carries what the buttons are enabled by, so a
  // drive that has just been taken by a job greys out at once.
  if (state.detail) {
    Object.assign(state.detail, {
      disc: d.disc, busy: d.busy, canBurn: d.canBurn, burnBlocker: d.burnBlocker,
      canRipIso: d.canRipIso, canRipImg: d.canRipImg, canRipAudio: d.canRipAudio,
    });
    renderDiscTab(state.detail);
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
    el('conn').textContent = 'live - every browser watching this server sees the same thing';
  });
  es.addEventListener('message', (ev) => {
    try {
      const snap = JSON.parse(ev.data);
      applySnapshot(snap);
      watchJobsForImages();
    } catch (e) { /* a malformed frame is not worth tearing the page down for */ }
  });
  es.addEventListener('error', () => {
    el('conn').textContent = 'reconnecting…';
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
  await Promise.all([loadImages(), loadFormats(), loadDiscs()]);
  connect();
}

main();
