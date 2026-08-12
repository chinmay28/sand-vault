#!/usr/bin/env node
/**
 * The one place the app's version number is assembled.
 *
 * Scheme: vMAJOR.MINOR.PATCH, where PATCH is the repository's commit count —
 * every commit is a patch release, so `v2.0.311` is the 311th commit on the
 * 2.0 line.
 *
 *   - MAJOR/MINOR are source constants, read out of
 *     internal/version/version.go so there is exactly one declaration of them
 *     in the tree. Bump them there.
 *   - PATCH comes from `git rev-list --count HEAD`, which only exists at build
 *     time: the Go binary gets it stamped in via -ldflags, the web bundle gets
 *     it inlined by Vite. Both call this file, so they can never disagree.
 *
 * Usage:
 *   node scripts/version.mjs            # print e.g. v2.0.311
 *   node scripts/version.mjs --patch    # print just the commit count (311)
 *   import { appVersion } from './scripts/version.mjs'
 */
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const GO_VERSION_FILE = resolve(repoRoot, 'internal/version/version.go');

/** Read `Major`/`Minor` out of the Go source that declares them. */
function majorMinor() {
  const src = readFileSync(GO_VERSION_FILE, 'utf8');
  const read = (name) => {
    const m = new RegExp(`^\\s*${name}\\s*=\\s*(\\d+)\\s*$`, 'm').exec(src);
    if (!m) {
      throw new Error(`could not find ${name} in ${GO_VERSION_FILE}`);
    }
    return Number(m[1]);
  };
  return { major: read('Major'), minor: read('Minor') };
}

/** Run git in the repo root; null if it fails (no repo, no git, old git). */
function git(args) {
  try {
    return execFileSync('git', args, {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
  } catch {
    return null;
  }
}

/**
 * The commit count on HEAD, or '0' when it can't be known — no repo (a tarball,
 * or a `COPY` that skipped `.git`), no git, or a **shallow** clone.
 *
 * Shallow is the trap, and it's why this isn't a bare `rev-list`: a clone made
 * with `--depth 1` answers `rev-list --count HEAD` with `1`, which is not an
 * error and not obviously wrong — it just quietly ships a build calling itself
 * `2.0.1`. Refuse it. Patch 0 is the agreed "unstamped build" marker (it
 * matches the Go default), and a version ending in `.0` is visibly a
 * non-release rather than a plausible lie.
 *
 * Anything building a release therefore needs the full commit graph:
 * `fetch-depth: 0` on GitHub Actions, `--filter=blob:none` rather than
 * `--depth 1` for a cheap clone that still carries all of it.
 */
export function commitCount() {
  if (git(['rev-parse', '--is-shallow-repository']) === 'true') {
    process.emitWarning(
      'shallow git clone — the commit count is not the real one, reporting patch 0. ' +
        'Clone with --filter=blob:none (or fetch --unshallow) for a real version.',
    );
    return '0';
  }
  // A failed probe (git older than 2.15, or no repo at all) is not proof of
  // shallowness — fall through and let the count itself answer.
  return git(['rev-list', '--count', 'HEAD']) ?? '0';
}

/**
 * The full version string, `v`-prefixed to match how the project tags releases
 * (v2.0.0). Must stay byte-identical to version.String() in the Go package.
 *
 * Note `--patch` / commitCount() stays bare: that one feeds `-ldflags -X` as
 * the value of `version.Patch`, which is the number alone.
 */
export function appVersion() {
  const { major, minor } = majorMinor();
  return `v${major}.${minor}.${commitCount()}`;
}

// Invoked directly (by the build scripts), print rather than export.
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.stdout.write(process.argv.includes('--patch') ? commitCount() : appVersion());
  process.stdout.write('\n');
}
