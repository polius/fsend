#!/usr/bin/env python3
"""release-notes.py: the GitHub release body, taken from CHANGELOG.md.

A release's notes are written by hand in CHANGELOG.md. This prints the
section for one tag, ready to hand to `goreleaser release
--release-notes <file>`, and fails with a message that says what to do
when the tag has no section, so a release can't go out with an empty or
generated body.

    python3 scripts/release-notes.py v1.17.0     # that tag's notes
    python3 scripts/release-notes.py             # the tag being released
    python3 scripts/release-notes.py --check     # lint CHANGELOG.md (CI)

Without a tag it takes the one the release workflow runs on
(GITHUB_REF_NAME when GITHUB_REF_TYPE is "tag"), or else the v* tag that
points at HEAD.

What comes out:
  - the body of "## <version>" (the heading itself is left out: the
    release title already names the version);
  - each wrapped paragraph and list item on one line, since a release
    page shows a newline inside a paragraph as a line break;
  - relative links made absolute and pinned to the tag, since a release
    page resolves them against /releases/tag/, where they would break;
  - a short footer: where to install, how to verify a download, and the
    link that compares this tag with the previous one.

A heading matches a tag when it names the same version: "## 1.17.0",
"## v1.17.0" and "## [1.17.0] - 2026-11-20" all match v1.17.0.
"## Unreleased" matches nothing.
"""
import argparse
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_REPO = 'polius/fsend'

TAG = re.compile(r'^v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$')
HEADING = re.compile(r'^##[ \t]+(.+?)[ \t]*$')
FENCE = re.compile(r'^[ \t]*(```|~~~)')
# "1.17.0", "v1.17.0", "[1.17.0]", each with an optional date after it.
VERSION_HEADING = re.compile(
    r'^\[?v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)\]?'
    r'(?:[ \t]*(?:[-\u2013\u2014][ \t]*)?\(?\d{4}-\d{2}-\d{2}\)?)?$')
# Inline links and images "](target)", and reference definitions
# "[name]: target".
INLINE_LINK = re.compile(r'\]\(([^)\s]+)((?:\s+"[^"]*")?)\)')
REF_LINK = re.compile(r'^(\s*\[[^\]]+\]:\s+)(\S+)(.*)$')
ABSOLUTE = re.compile(r'^(?:[a-z][a-z0-9+.-]*:|//)', re.I)
CODE_SPAN = re.compile(r'(`+[^`]*`+)')
# A line that starts a new block, so it never continues the paragraph
# above it: a list item, heading, quote, table row, fence or rule.
ONE_LINE = r'#{1,6}[ \t]|\||(?:-{3,}|\*{3,}|_{3,})[ \t]*$'
BLOCK_START = re.compile(r'^[ \t]*(?:[-*+][ \t]|\d+[.)][ \t]|>|```|~~~|' + ONE_LINE + ')')
# Headings, table rows and rules are one line each: nothing joins them.
SINGLE_LINE = re.compile(r'^[ \t]*(?:' + ONE_LINE + ')')


class Problem(Exception):
    """A message for the person cutting the release."""


def repo_url():
    server = os.environ.get('GITHUB_SERVER_URL', 'https://github.com').rstrip('/')
    return server + '/' + os.environ.get('GITHUB_REPOSITORY', DEFAULT_REPO)


def sections(text):
    """[(heading text, version or None, body lines)] for every "## "
    heading, in file order. Headings inside code fences don't count."""
    out, fence = [], None
    for line in text.splitlines():
        f = FENCE.match(line)
        if f:
            if fence is None:
                fence = f.group(1)
            elif fence == f.group(1):
                fence = None
        m = HEADING.match(line) if fence is None else None
        if m:
            v = VERSION_HEADING.match(m.group(1))
            out.append((m.group(1), v.group(1) if v else None, []))
        elif out:
            out[-1][2].append(line)
    return out


def trim(lines):
    lines = list(lines)
    while lines and not lines[0].strip():
        lines.pop(0)
    while lines and not lines[-1].strip():
        lines.pop()
    return lines


def absolute(target, tag, changelog):
    """A link target as it must read on the release page."""
    if ABSOLUTE.match(target):
        return target
    base = f'{repo_url()}/blob/{tag}/'
    if target.startswith('#'):
        return base + changelog + target
    # Targets are relative to the changelog's folder.
    path = os.path.normpath(os.path.join(os.path.dirname(changelog), target.lstrip('/')))
    return base + path.replace(os.sep, '/')


def unwrap(lines):
    """Join the lines of each wrapped paragraph and list item into one.
    The changelog is wrapped at 72 columns, and a release page shows
    every newline in a paragraph as a line break."""
    out, fence, open_par = [], None, False
    for line in lines:
        f = FENCE.match(line)
        if f:
            if fence is None:
                fence = f.group(1)
            elif fence == f.group(1):
                fence = None
            out.append(line)
            open_par = False
            continue
        if fence is not None:
            out.append(line)
            continue
        if not line.strip():
            out.append(line)
            open_par = False
        else:
            # A deliberate break (two trailing spaces, or a backslash)
            # is kept, and ends the join.
            hard = line.endswith(('  ', '\\'))
            if open_par and not BLOCK_START.match(line):
                out[-1] += ' ' + (line.lstrip() if hard else line.strip())
            else:
                out.append(line if hard else line.rstrip())
            open_par = not hard and not SINGLE_LINE.match(line)
    return out


def pin_links(lines, tag, changelog):
    out, fence = [], None
    for line in lines:
        f = FENCE.match(line)
        if f:
            if fence is None:
                fence = f.group(1)
            elif fence == f.group(1):
                fence = None
            out.append(line)
            continue
        if fence is None:
            # Not inside `code spans`: the odd pieces of the split.
            parts = CODE_SPAN.split(line)
            for i in range(0, len(parts), 2):
                parts[i] = INLINE_LINK.sub(
                    lambda m: f']({absolute(m.group(1), tag, changelog)}{m.group(2)})', parts[i])
            line = ''.join(parts)
            r = REF_LINK.match(line)
            if r:
                line = r.group(1) + absolute(r.group(2), tag, changelog) + r.group(3)
        out.append(line)
    return out


def git(*args):
    try:
        return subprocess.run(('git',) + args, cwd=ROOT, capture_output=True,
                              text=True, check=True).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        return None


def previous_tag(tag):
    """The nearest v* tag before this one, or None (no git, a shallow
    clone, the first release). A tag that doesn't exist yet is taken to
    be going on HEAD, so the notes can be read before tagging."""
    start = tag + '^' if git('rev-parse', '-q', '--verify', 'refs/tags/' + tag) else 'HEAD'
    out = git('describe', '--tags', '--abbrev=0', '--match', 'v[0-9]*', start)
    return out if out and out != tag and TAG.match(out) else None


def footer(tag, prev):
    url = repo_url()
    lines = [
        '---',
        '',
        f'[Install]({url}/tree/{tag}#install) · '
        f'[Verify a download]({url}/blob/{tag}/docs/security.md#release-integrity) · '
        f'[Version compatibility]({url}/blob/{tag}/docs/compatibility.md)',
    ]
    if prev:
        lines += ['', f'**Full changelog**: {url}/compare/{prev}...{tag}']
    return lines


def notes(text, tag, changelog='CHANGELOG.md', prev=None):
    m = TAG.match(tag)
    if not m:
        raise Problem(f'"{tag}" is not a release tag (expected vMAJOR.MINOR.PATCH, like v1.17.0).')
    version = m.group(1)
    found = sections(text)
    body = [b for _, v, b in found if v == version]
    if not body:
        have = ', '.join(h for h, _, _ in found) or 'none'
        raise Problem(
            f'{changelog} has no "## {version}" section for tag {tag}.\n'
            f'The GitHub release body is that section, so the release stops here.\n'
            f'Rename "## Unreleased" to "## {version}" (or write the section), commit,\n'
            f'and tag that commit. Sections found: {have}.')
    if len(body) > 1:
        raise Problem(f'{changelog} has {len(body)} sections for {version}; keep one.')
    lines = trim(body[0])
    if not lines:
        raise Problem(f'The "## {version}" section of {changelog} is empty; write the release notes there.')
    return '\n'.join(pin_links(unwrap(lines), tag, changelog) + [''] + footer(tag, prev)) + '\n'


def version_key(v):
    core, _, pre = v.partition('-')
    return tuple(int(x) for x in core.split('.')), pre == '', pre


def check(text, changelog):
    """Lint: every "## " heading is "Unreleased" or a version, versions
    are unique and newest first, no released section is empty, and the
    newest one extracts."""
    found = sections(text)
    if not found:
        raise Problem(f'{changelog} has no "## " sections.')
    versions = []
    for i, (heading, version, body) in enumerate(found):
        if version is None:
            if heading.lower() != 'unreleased':
                raise Problem(f'{changelog}: "## {heading}" is neither "Unreleased" nor a version like "1.17.0".')
            if i != 0:
                raise Problem(f'{changelog}: "## {heading}" must be the first section.')
            continue
        if not trim(body):
            raise Problem(f'{changelog}: the "## {heading}" section is empty.')
        versions.append(version)
    if len(set(versions)) != len(versions):
        raise Problem(f'{changelog}: a version has two sections.')
    if versions != sorted(versions, key=version_key, reverse=True):
        raise Problem(f'{changelog}: versions are not newest first: {", ".join(versions)}.')
    if versions:
        notes(text, 'v' + versions[0], changelog)
    return found


def current_tag():
    if os.environ.get('GITHUB_REF_TYPE') == 'tag' and os.environ.get('GITHUB_REF_NAME'):
        return os.environ['GITHUB_REF_NAME']
    out = (git('tag', '--points-at', 'HEAD', '--sort=-version:refname', '--list', 'v[0-9]*') or '').split()
    if not out:
        raise Problem('No tag given, and no v* tag points at HEAD: run the release on a tag (vMAJOR.MINOR.PATCH).')
    return out[0]


def main():
    ap = argparse.ArgumentParser(description='Print the release notes for a tag from CHANGELOG.md.')
    ap.add_argument('tag', nargs='?', help='the release tag, like v1.17.0 (default: the tag being released)')
    ap.add_argument('--changelog', default=os.path.join(ROOT, 'CHANGELOG.md'), help='path to the changelog')
    ap.add_argument('--check', action='store_true', help='lint the changelog and exit')
    args = ap.parse_args()
    # The changelog's path in the repository, which links are pinned
    # against. A copy elsewhere (a dry run) stands in for the real one.
    name = os.path.relpath(os.path.abspath(args.changelog), ROOT).replace(os.sep, '/')
    if name.startswith('../'):
        name = 'CHANGELOG.md'
    try:
        with open(args.changelog, encoding='utf-8') as f:
            text = f.read()
        if args.check:
            found = check(text, name)
            print(f'{name}: ok ({", ".join(h for h, _, _ in found)})')
            return 0
        tag = args.tag or current_tag()
        sys.stdout.write(notes(text, tag, name, previous_tag(tag)))
        return 0
    except (Problem, OSError) as e:
        msg = str(e)
        if os.environ.get('GITHUB_ACTIONS') == 'true':
            # One annotation on the run's summary page; %0A is a newline.
            # On stderr: stdout is the notes file.
            print(f'::error file={name},title=Release notes::' + msg.replace('%', '%25').replace('\n', '%0A'),
                  file=sys.stderr)
        print('release-notes: ' + msg, file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
