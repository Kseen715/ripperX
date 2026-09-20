'use strict';

// Every word these pages show comes out of a locale file rather than out of
// the markup or the code, so adding a language means adding one file and
// changing nothing else. The files live beside the pages and are served as
// they are; the list of them and which one this server prefers come from
// /api/locales, which is readable without signing in because the sign-in
// page needs its own words too.
//
// Three dictionaries are consulted in turn: the one the reader chose, the
// one this server was configured with, and English. A string missing from a
// half-finished translation therefore appears in the server's language, or
// failing that in English, rather than as a gap - which is what makes it
// possible to add a language a few lines at a time.

const i18n = (() => {
  const builtinFallback = 'en';
  const stored = 'ripperx.lang';

  const state = {
    code: builtinFallback,
    serverDefault: builtinFallback,
    languages: [{ code: builtinFallback, name: 'English' }],
    // The three dictionaries, most specific first.
    chain: [],
  };

  // Plural rules, by the name a locale file declares. English wants two
  // forms and Russian three, and a language that says nothing gets the
  // two-form rule - which is wrong for some languages and is at least
  // wrong in a way its translator can fix by naming a rule here.
  const pluralRules = {
    'one-other': (n) => (n === 1 ? 'one' : 'other'),
    russian: (n) => {
      const mod10 = n % 10, mod100 = n % 100;
      if (mod10 === 1 && mod100 !== 11) return 'one';
      if (mod10 >= 2 && mod10 <= 4 && (mod100 < 12 || mod100 > 14)) return 'few';
      return 'many';
    },
  };

  function lookup(key) {
    for (const dict of state.chain) {
      const v = dict[key];
      if (v !== undefined) return v;
    }
    // A key with no string anywhere shows as itself. It is not a nice thing
    // to read, and that is the point: it is a bug, and it should look like
    // one rather than like an empty label.
    return key;
  }

  // fill substitutes {name} placeholders. A name with no value is left
  // alone, so a mistake in a translation shows what was expected.
  function fill(s, vars) {
    if (!vars) return s;
    return String(s).replace(/\{(\w+)\}/g, (whole, name) =>
      vars[name] === undefined || vars[name] === null ? whole : String(vars[name]));
  }

  function t(key, vars) { return fill(lookup(key), vars); }

  // tn picks the form that goes with a count and passes the count in as
  // {n}, so a translation writes "{n} discs" without the caller knowing how
  // many forms the language has.
  function tn(key, n, vars) {
    const rule = pluralRules[lookup('_plural')] || pluralRules['one-other'];
    const form = rule(Math.abs(Number(n) || 0));
    let s = lookup(`${key}.${form}`);
    if (s === `${key}.${form}`) s = lookup(`${key}.other`);
    return fill(s, Object.assign({ n }, vars));
  }

  // apply writes every string into the markup that asked for one. The
  // attributes are separate because a placeholder and a title are not text
  // nodes and setting them as one would put the words in the wrong place.
  function apply(root) {
    const scope = root || document;
    const put = (attr, set) => {
      for (const node of scope.querySelectorAll(`[${attr}]`)) {
        set(node, t(node.getAttribute(attr)));
      }
    };
    put('data-i18n', (n, s) => { if (n.textContent !== s) n.textContent = s; });
    put('data-i18n-placeholder', (n, s) => { n.placeholder = s; });
    put('data-i18n-title', (n, s) => { n.title = s; });
    put('data-i18n-label', (n, s) => { n.setAttribute('aria-label', s); });
  }

  async function fetchLocale(code) {
    try {
      const r = await fetch(`/locales/${encodeURIComponent(code)}.json`, { cache: 'no-cache' });
      if (!r.ok) return null;
      return await r.json();
    } catch (e) {
      return null;
    }
  }

  // chosen is the reader's own preference: what the URL says, then what
  // this browser remembered, then what the server was configured with. The
  // URL comes first so a link can be sent in a particular language.
  function chosen() {
    const fromURL = new URLSearchParams(window.location.search).get('lang');
    if (fromURL) return fromURL;
    try {
      const saved = window.localStorage.getItem(stored);
      if (saved) return saved;
    } catch (e) { /* a browser with storage turned off still gets a language */ }
    return state.serverDefault;
  }

  function known(code) {
    return state.languages.some((l) => l.code === code);
  }

  async function start() {
    try {
      const r = await fetch('/api/locales', { cache: 'no-cache' });
      if (r.ok) {
        const cat = await r.json();
        if (cat.languages && cat.languages.length) state.languages = cat.languages;
        if (cat.default) state.serverDefault = cat.default;
      }
    } catch (e) { /* the built-in defaults are enough to render English */ }

    let code = chosen();
    if (!known(code)) code = state.serverDefault;
    if (!known(code)) code = builtinFallback;
    state.code = code;

    // Loaded most specific first, and each only once: a server whose
    // default is English asks for one file, not three.
    const wanted = [];
    for (const c of [code, state.serverDefault, builtinFallback]) {
      if (c && !wanted.includes(c)) wanted.push(c);
    }
    const dicts = await Promise.all(wanted.map(fetchLocale));
    state.chain = dicts.filter((d) => d);
    document.documentElement.lang = code;
    apply(document);
  }

  // choose switches language. The page is reloaded rather than re-rendered:
  // the strings are woven through every list, every dialog and every job
  // line on screen, and reloading is both simpler and provably complete.
  function choose(code) {
    if (code === state.code) return;
    try { window.localStorage.setItem(stored, code); } catch (e) { /* not fatal */ }
    const url = new URL(window.location.href);
    url.searchParams.delete('lang');
    window.location.replace(url.toString());
  }

  return {
    t, tn, apply, start, choose,
    get code() { return state.code; },
    get languages() { return state.languages; },
  };
})();

const t = (key, vars) => i18n.t(key, vars);
const tn = (key, n, vars) => i18n.tn(key, n, vars);
