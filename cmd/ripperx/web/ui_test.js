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
  .replace('<script src="app.js"></script>', '');

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
const images = { files: [{ name: 'disc.iso', size: 2048 * 100, modTime: '2026-01-01T00:00:00Z', needs: 'CD', aligned: true }], store: '//nas/images', kind: 'smb' };
const isos = {
  files: [{
    name: 'debian.iso', size: 2048 * 500, modTime: '2026-01-01T00:00:00Z',
    needs: 'DVD', aligned: true, volume: 'Debian', summary: 'boots on PC BIOS and UEFI, on x86_64',
    boot: { bootable: true, platforms: [{ id: 0, name: 'PC BIOS' }, { id: 239, name: 'UEFI' }], architectures: ['x86_64'] },
  }],
  store: '//nas/iso', kind: 'smb', readOnly: true,
};
const routes = {
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
    canBrowse: present, canBurn: false, burnBlocker: 'this disc cannot be written to',
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

const app = fs.readFileSync(path.join(web, 'app.js'), 'utf8');
try {
  window.eval(app);
} catch (e) {
  check('app.js evaluates', false, e.stack);
  process.exit(1);
}
check('app.js evaluates', true);

const doc = window.document;
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

  check('nothing threw', errors.length === 0, errors.join('\n'));
  console.log(failures ? `\n${failures} failed` : '\nall good');
  process.exit(failures ? 1 : 0);
})();
