#!/usr/bin/env python3
"""How much of the page each language has words for.

English is the source: every string the page can show has a key in en.json,
and every other locale file is a translation of that set. A language may be
incomplete on purpose - the page falls back to the server's default language
and then to English for anything missing - so this reports rather than
refuses, and the figure goes into the release notes so that "German is at
60%" is something a reader can see before installing.

Plural forms are counted as one key. English wants two forms of "1 disc" and
Russian three, so counting the forms would put Russian permanently over 100%
and a two-form language permanently under it; what is being measured is
whether the phrase has been translated, not how many endings it needed.

    scripts/i18n-coverage.py [--format md|text] [--fail-under N]
"""

import argparse
import json
import pathlib
import sys

LOCALES = pathlib.Path(__file__).resolve().parent.parent / "cmd/ripperx/web/locales"
SOURCE = "en"
PLURAL_FORMS = (".one", ".few", ".many", ".other")


def stem(key):
    for form in PLURAL_FORMS:
        if key.endswith(form):
            return key[: -len(form)]
    return key


def keys_of(path):
    """The translatable keys in one file.

    Anything beginning with an underscore is metadata - what the language
    calls itself, which plural rule it uses - and is not a string anybody
    reads, so it is not counted.
    """
    with open(path, encoding="utf-8") as f:
        data = json.load(f)
    if not isinstance(data, dict):
        raise SystemExit(f"{path}: a locale file is an object of key to string")
    return data, {stem(k) for k in data if not k.startswith("_")}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--format", choices=["md", "text"], default="text")
    ap.add_argument("--fail-under", type=float, default=None,
                    help="exit non-zero if any language is below this percentage")
    args = ap.parse_args()

    files = sorted(LOCALES.glob("*.json"))
    if not files:
        raise SystemExit(f"no locale files in {LOCALES}")
    source = LOCALES / f"{SOURCE}.json"
    if source not in files:
        raise SystemExit(f"{source} is missing, and it is the one every other file is measured against")

    _, want = keys_of(source)
    rows, worst, problems = [], 100.0, []
    for path in files:
        data, have = keys_of(path)
        code = path.stem
        done = len(want & have)
        extra = sorted(have - want)
        pct = 100.0 * done / len(want) if want else 100.0
        rows.append((code, data.get("_name", code), done, len(want), pct, extra))
        worst = min(worst, pct)
        if extra:
            problems.append(f"{code}.json has {len(extra)} key(s) en.json does not: "
                            + ", ".join(extra[:8]) + (" …" if len(extra) > 8 else ""))

    if args.format == "md":
        print("| Language | Translated |")
        print("| --- | --- |")
        for code, name, done, total, pct, _ in rows:
            bar = "100%" if done == total else f"{pct:.0f}%"
            print(f"| {name} (`{code}`) | {bar} — {done}/{total} strings |")
    else:
        width = max(len(f"{n} ({c})") for c, n, *_ in rows)
        for code, name, done, total, pct, _ in rows:
            print(f"{name} ({code})".ljust(width) + f"  {pct:6.1f}%  {done}/{total}")

    for line in problems:
        print(f"::warning::{line}", file=sys.stderr)

    if args.fail_under is not None and worst < args.fail_under:
        print(f"the least complete language is at {worst:.1f}%, "
              f"under the {args.fail_under}% this build asks for", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
