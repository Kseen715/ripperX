// The page's own test. It loads index.html and app.js into a DOM, feeds them
// the snapshots the server pushes, and checks what cannot be checked by
// reading the code: that a snapshot does not rebuild the page.
//
// This exists because of a specific bug. Every snapshot - twice a second -
// rebuilt the drive list, the job list and the whole disc panel, so the row
// under the cursor was destroyed and created again between the press of a
// mouse button and its release, an open menu closed itself, and the <audio>
// element playing a track was replaced along with everything else. None of
// that shows up in a screenshot, and all of it shows up here: the test marks
// nodes, pushes twenty snapshots, and looks for its marks.
//
//     npm install jsdom && node cmd/ripperx/web/ui_test.js
const fs = require('fs');
const path = require('path');
const { JSDOM, VirtualConsole } = require('jsdom');

const web = __dirname;
const html = fs.readFileSync(path.join(web, 'index.html'), 'utf8')
  .replace('<script src="i18n.js"></script>', '')
  .replace('<script src="app.js"></script>', '');

// The page's words live in locale files the server serves. The test reads
// them off disk and hands them to the page through the same two URLs, so a
// key added to the code and forgotten in the file shows up here rather than
// on somebody's screen.
const locales = {};
for (const file of fs.readdirSync(path.join(web, 'locales'))) {
  if (file.endsWith('.json')) {
    locales[file.replace('.json', '')] =
      JSON.parse(fs.readFileSync(path.join(web, 'locales', file), 'utf8'));
  }
}

let failures = 0;
function check(name, ok, detail) {
  console.log(`${ok ? 'ok  ' : 'FAIL'}  ${name}${ok || !detail ? '' : `\n        ${detail}`}`);
  if (!ok) failures++;
}

const errors = [];
const vc = new VirtualConsole();
vc.on('jsdomError', (e) => errors.push(String(e.message)));
vc.on('error', (e) => errors.push(String(e)));

const dom = new JSDOM(html, { runScripts: 'outside-only', virtualConsole: vc, url: 'http://localhost/' });
const { window } = dom;

// The page talks to three things: fetch, EventSource and XMLHttpRequest.
const status = {
  version: 'test', drives: 1, store: '//nas/images', storeKind: 'smb',
  burner: '/usr/bin/xorriso', burnerKind: 'xorriso', allowBurn: true,
  allowEject: true, authOn: false, isoStore: '//nas/iso', uploadMax: 1 << 30,
};
// A store with hundreds of files in it, which is the case the images panel
// has to survive, plus the archive a ripped disc becomes.
const manyImages = [];
for (let i = 1; i <= 250; i++) {
  manyImages.push({
    name: `img-${String(i).padStart(4, '0')}.iso`, size: 2048 * (100 + i),
    modTime: '2026-01-01T00:00:00Z', needs: 'CD', aligned: true,
  });
}
manyImages.push({
  name: 'Terminator-5v1-20260919.zip', size: 3_900_000_000,
  modTime: '2026-01-02T00:00:00Z', needs: 'dual-layer DVD',
});
const images = {
  files: manyImages, store: '//nas/images', kind: 'smb',
  free: 412 * 1024 ** 3, total: 3.6 * 1024 ** 4,
};
const isos = {
  files: [{
    name: 'debian.iso', size: 2048 * 500, modTime: '2026-01-01T00:00:00Z',
    needs: 'DVD', aligned: true, volume: 'Debian', summary: 'boots on PC BIOS and UEFI, on x86_64',
    boot: { bootable: true, platforms: [{ id: 0, name: 'PC BIOS' }, { id: 239, name: 'UEFI' }], architectures: ['x86_64'] },
  }],
  store: '//nas/iso', kind: 'smb', readOnly: true,
};
const routes = {
  '/api/locales': {
    default: 'en',
    languages: Object.entries(locales).map(([code, d]) => ({ code, name: d._name })),
  },
  '/api/status': status,
  '/api/images': images,
  '/api/isos': isos,
  '/api/formats': { formats: [{ id: 'zip', name: 'Zip', extension: '.zip', note: 'everything opens it' }] },
  '/api/discs': { enabled: true, discs: [] },
};
let fetches = 0;
window.fetch = async (url) => {
  fetches++;
  const p = String(url).split('?')[0];
  let body = routes[p];
  if (p.startsWith('/locales/')) {
    body = locales[p.slice('/locales/'.length).replace('.json', '')] || {};
  }
  if (p.startsWith('/api/drives/') && p.endsWith('/browse')) {
    body = { path: '/', parent: '', volume: { volumeId: 'DMC_STRA' }, entries: [] };
  } else if (p.startsWith('/api/drives/') && !p.includes('/', 12 + 3)) {
    body = detail(loaded);
  }
  if (!body) body = {};
  return { ok: true, status: 200, json: async () => body };
};
let sse = null;
window.EventSource = class {
  constructor() { sse = this; this.listeners = {}; }
  addEventListener(k, fn) { (this.listeners[k] ||= []).push(fn); }
  emit(k, ev) { for (const fn of this.listeners[k] || []) fn(ev); }
};
window.XMLHttpRequest = class { open() {} send() {} setRequestHeader() {} upload = {}; };

// What is in the drive right now, so the detail endpoint answers the same
// thing the event stream is saying.
let loaded = true;
let burnable = false;

function disc(present = true) {
  return present ? {
    present: true, profile: 0x10, profileName: 'DVD-ROM', statusName: 'complete',
    sectors: 2023168, dataBytes: 4143448064, dataTracks: 1, audioTracks: 0,
    sessions: 1, tracks: [{ number: 1, audio: false }], writableBytes: 0,
  } : { present: false, profileName: 'no disc', sectors: 0 };
}
function drive(present = true, busy = false) {
  return {
    id: 'sr0', path: '/dev/sr0', name: 'HL-DT-ST BD-RE', busy,
    disc: disc(present), canRipIso: present, canRipImg: false, canRipAudio: false,
    canBrowse: present, canBurn: burnable, burnBlocker: 'this disc cannot be written to',
    canAppend: false, appendBlocker: 'this disc is closed',
  };
}
function detail(present = true) {
  return Object.assign(drive(present), {
    volume: present ? { volumeId: 'DMC_STRA', format: 'UDF', joliet: false, rockRidge: false } : null,
    capabilities: { info: { vendor: 'HL-DT-ST', product: 'BD-RE', version: '1.00' }, can: ['read DVD discs'], cannot: ['write BD-R'], notes: [] },
  });
}
function snapshot(jobs) { return { drives: [drive()], jobs: jobs || [] }; }

const app = fs.readFileSync(path.join(web, 'i18n.js'), 'utf8') + '\n' +
  fs.readFileSync(path.join(web, 'app.js'), 'utf8');
try {
  window.eval(app);
} catch (e) {
  check('app.js evaluates', false, e.stack);
  process.exit(1);
}
check('app.js evaluates', true);

const doc = window.document;

// jsdom has <dialog> as an element but not its modal behaviour, and does not
// submit a form[method=dialog] on a button press. Both are supplied here so
// the test exercises the page's own logic rather than the browser's: what is
// being checked is that nothing irreversible happens without being asked,
// not that <dialog> works.
for (const dlg of doc.querySelectorAll('dialog')) {
  dlg.showModal = function () { this.open = true; };
  dlg.close = function (value) {
    if (value !== undefined) this.returnValue = value;
    this.open = false;
    this.dispatchEvent(new window.Event('close'));
  };
}
for (const b of doc.querySelectorAll('dialog form[method=dialog] button')) {
  b.addEventListener('click', (ev) => {
    ev.preventDefault();
    b.closest('dialog').close(b.value);
  });
}
const push = (snap) => sse.emit('message', { data: JSON.stringify(snap) });
const settle = () => new Promise((r) => setTimeout(r, 30));

(async () => {
  await settle();
  // The drives arrive on the event stream, the way they do from the server.
  push(snapshot([]));
  await settle();
  await settle();
  check('a drive row was drawn', !!doc.querySelector('.drive'), doc.getElementById('drives').innerHTML);
  check('the disc panel is open', !doc.getElementById('work').hidden);
  check('the disc is named', doc.getElementById('discTitle').textContent === 'DMC_STRA',
    doc.getElementById('discTitle').textContent);
  check('the pane bar was built', doc.getElementById('seg').children.length > 0);

  // The claim: identity survives a snapshot. Mark the nodes, push ten
  // snapshots with a job ticking along, and see whether the marks are still
  // on the same elements.
  const driveRow = doc.querySelector('.drive');
  driveRow.dataset.mark = 'x';
  const segBtn = doc.getElementById('seg').children[0];
  segBtn.dataset.mark = 'x';

  push(snapshot([{ id: 'j1', kind: 'rip', drive: 'sr0', label: 'disc.iso', state: 'running', done: 1, total: 100, started: new Date().toISOString(), targets: [] }]));
  await settle();
  const jobNode = doc.querySelector('#jobs .job');
  check('a running job was drawn', !!jobNode);
  if (jobNode) jobNode.dataset.mark = 'x';
  const barFill = jobNode && jobNode.querySelector('.bar > i');

  for (let i = 2; i <= 40; i += 2) {
    push(snapshot([{ id: 'j1', kind: 'rip', drive: 'sr0', label: 'disc.iso', state: 'running', done: i, total: 100, bytesPerSec: 4e6, started: new Date().toISOString(), targets: [] }]));
    await settle();
  }

  check('the drive row is the same element after 20 snapshots',
    doc.querySelector('.drive') && doc.querySelector('.drive').dataset.mark === 'x');
  check('the pane button is the same element',
    doc.getElementById('seg').children[0].dataset.mark === 'x');
  check('the job row is the same element',
    doc.querySelector('#jobs .job') && doc.querySelector('#jobs .job').dataset.mark === 'x');
  check('the progress bar moved', barFill && parseFloat(barFill.style.width) === 40, barFill && barFill.style.width);
  check('the percentage has settled text', doc.querySelector('#jobs .pct').textContent === '40%',
    doc.querySelector('#jobs .pct').textContent);

  // A finished job leaves the running list and lands behind the count.
  push(snapshot([{ id: 'j1', kind: 'rip', drive: 'sr0', label: 'disc.iso', state: 'done', done: 100, total: 100, started: new Date().toISOString(), sha256: 'abc', targets: ['disc.iso'] }]));
  await settle();
  check('finished jobs leave the running list', doc.querySelectorAll('#jobs .job').length === 0);
  check('finished jobs are counted behind a disclosure',
    doc.getElementById('doneSummary').textContent === '1 finished job',
    doc.getElementById('doneSummary').textContent);
  check('the finished job is still reachable', doc.querySelectorAll('#doneJobs .job').length === 1);

  // Switching panes.
  const before = fetches;
  const segs = Array.from(doc.getElementById('seg').children).map((b) => b.dataset.key);
  check('the panes on offer fit the disc', JSON.stringify(segs) === JSON.stringify(['rip', 'files', 'check', 'burn', 'drive']),
    JSON.stringify(segs));
  doc.getElementById('seg').querySelector('[data-key=drive]').click();
  await settle();
  check('switching pane shows it', !doc.getElementById('paneDrive').hidden && doc.getElementById('paneRip').hidden);
  check('the drive pane was filled', doc.getElementById('can').children.length === 1);

  // Twenty more snapshots while a pane is open must not fetch anything.
  const quiet = fetches;
  for (let i = 0; i < 20; i++) { push(snapshot([])); await settle(); }
  check('snapshots do not fetch', fetches === quiet, `${fetches - quiet} requests`);

  // An empty drive: the panes that cannot apply go away.
  loaded = false;
  push({ drives: [drive(false)], jobs: [] });
  await settle();
  await settle();
  const segs2 = Array.from(doc.getElementById('seg').children).map((b) => b.dataset.key);
  check('an empty drive offers only what applies', !segs2.includes('rip') && !segs2.includes('files'),
    JSON.stringify(segs2));

  // ---- a store with hundreds of files in it ----
  loaded = true;
  push(snapshot([]));
  await settle();
  // How many rows make a page is the page's business; what is checked is
  // that there is a limit, that the count says so, and that the rest can
  // still be had. Pinning the number here would mean this test had an
  // opinion about a layout choice, and would break when that changed.
  const drawn = doc.querySelectorAll('#imageRows tr').length;
  check('the image list is capped rather than drawn in full',
    drawn > 0 && drawn < manyImages.length, `${drawn} of ${manyImages.length} rows`);
  check('the count says what is being shown',
    doc.getElementById('imageCount').textContent === `${drawn} of 251 images`,
    doc.getElementById('imageCount').textContent);
  doc.getElementById('imageMore').click();
  await settle();
  check('the rest can be asked for',
    doc.querySelectorAll('#imageRows tr').length === 251,
    `${doc.querySelectorAll('#imageRows tr').length} rows`);

  const filter = doc.getElementById('imageFilter');
  filter.value = 'terminator';
  filter.dispatchEvent(new window.Event('input'));
  await settle();
  check('the filter narrows the list',
    doc.querySelectorAll('#imageRows tr').length === 1,
    `${doc.querySelectorAll('#imageRows tr').length} rows`);
  check('the count says how much was filtered out',
    doc.getElementById('imageCount').textContent === '1 of 251 images',
    doc.getElementById('imageCount').textContent);
  filter.value = '';
  filter.dispatchEvent(new window.Event('input'));
  await settle();

  check('the space left on the store is shown',
    doc.getElementById('storeFree').textContent === '412 GB free of 3.6 TB',
    doc.getElementById('storeFree').textContent);
  check('and is not marked as low when it is not',
    !doc.getElementById('storeFree').classList.contains('low'));

  // ---- an archive is burnable, as the files inside it ----
  burnable = true;
  push(snapshot([]));
  await settle();
  await settle();
  doc.getElementById('seg').querySelector('[data-key=burn]').click();
  await settle();
  doc.getElementById('burnPick').click();
  await settle();
  const rows = Array.from(doc.querySelectorAll('#burnList .pick-opt'));
  const archiveRow = rows.find((r) => r.textContent.includes('Terminator-5v1-20260919.zip'));
  check('an archive is offered in the burn menu', !!archiveRow,
    `${rows.length} rows offered`);
  check('and is marked as what will happen to it',
    archiveRow && archiveRow.textContent.includes('unpacked to files'),
    archiveRow && archiveRow.textContent);

  archiveRow.click();
  await settle();
  check('choosing it says the disc gets the files',
    doc.getElementById('burnBoot').textContent.includes('files inside it'),
    doc.getElementById('burnBoot').textContent);

  // ---- naming what comes out ----
  doc.getElementById('seg').querySelector('[data-key=files]').click();
  await settle();
  check('the take dialog asks for a name', !!doc.getElementById('archiveName'));

  // ---- nothing irreversible happens without being asked ----
  filter.value = 'Terminator';
  filter.dispatchEvent(new window.Event('input'));
  await settle();
  const deletes = [];
  const realFetch = window.fetch;
  window.fetch = async (url, opts) => {
    if (opts && opts.method === 'DELETE') deletes.push(String(url));
    return realFetch(url, opts);
  };
  const row = doc.querySelector('#imageRows tr');
  const del = Array.from(row.querySelectorAll('button')).find((b) => b.textContent === 'Delete');
  check('an image can be deleted', !!del);

  del.click();
  await settle();
  const dlg = doc.getElementById('confirmDialog');
  check('deleting asks first', dlg.open === true);
  check('and says what it is about to delete',
    doc.getElementById('confirmWhat').textContent.includes('Terminator'),
    doc.getElementById('confirmWhat').textContent);
  check('and that it cannot be undone',
    !doc.getElementById('confirmWarn').hidden);
  check('with the safe button holding the focus',
    doc.activeElement === doc.getElementById('confirmNo'),
    doc.activeElement && doc.activeElement.id);

  doc.getElementById('confirmNo').click();
  await settle();
  check('saying no deletes nothing', deletes.length === 0, deletes.join(', '));

  del.click();
  await settle();
  doc.getElementById('confirmGo').click();
  await settle();
  check('saying yes deletes it', deletes.length === 1, `${deletes.length} requests`);
  window.fetch = realFetch;

  check('nothing threw', errors.length === 0, errors.join('\n'));
  // ---- every word on the page came out of a locale file ----
  // A key with no string anywhere renders as the key itself, which is a
  // shape no sentence has: lower-case words joined by dots and nothing else.
  // Sweeping the rendered page for that catches a string added to the code
  // and forgotten in en.json, which is otherwise invisible until a reader
  // meets it.
  const missing = new Set();
  const keyish = /^[a-z][a-zA-Z0-9]*(\.[a-zA-Z0-9]+)+$/;
  const walk = (node) => {
    for (const child of node.childNodes) {
      if (child.nodeType === 3) {
        const text = child.textContent.trim();
        if (keyish.test(text)) missing.add(text);
      } else if (child.nodeType === 1) {
        for (const attr of ['placeholder', 'title', 'aria-label']) {
          const v = child.getAttribute && child.getAttribute(attr);
          if (v && keyish.test(v.trim())) missing.add(v.trim());
        }
        walk(child);
      }
    }
  };
  walk(doc.body);
  check('every string on the page has a translation', missing.size === 0,
    Array.from(missing).join(', '));

  // English is the source every other file is a translation of, so a key
  // it does not have is a key nothing can fall back to.
  //
  // A key another language is *missing* is not an error: the whole point of
  // the fallback chain is that a language can be added a few lines at a
  // time, and the CI job reports how far each one has got. A key another
  // language has and English does not is a different thing - a typo, or a
  // string deleted from the code and left behind - and that does fail.
  // A key beginning with an underscore is metadata - what the language calls
  // itself, which plural rule it uses - and not a string anybody reads.
  const stem = (k) => k.replace(/\.(one|few|many|other)$/, '');
  const translatable = (dict) =>
    new Set(Object.keys(dict).filter((k) => !k.startsWith('_')).map(stem));
  const enKeys = translatable(locales.en);
  for (const [code, dict] of Object.entries(locales)) {
    if (code === 'en') continue;
    const have = translatable(dict);
    const extra = Array.from(have).filter((k) => !enKeys.has(k));
    const gaps = Array.from(enKeys).filter((k) => !have.has(k));
    check(`${code}.json has no keys en.json does not`, extra.length === 0,
      extra.join(', '));
    const done = enKeys.size - gaps.length;
    console.log(`      ${code}: ${Math.round((done / enKeys.size) * 100)}% ` +
      `translated (${done}/${enKeys.size})`);
  }

  console.log(failures ? `\n${failures} failed` : '\nall good');
  process.exit(failures ? 1 : 0);
})();
