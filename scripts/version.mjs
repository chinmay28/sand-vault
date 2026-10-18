#!/usr/bin/env node
/**
 * The one place the app's version number is assembled.
 *
 * Scheme: vYEAR.MONTH.PATCH — a calendar version, where PATCH is the
 * repository's commit count, so `v2026.8.311` is the 311th commit on the 2026.8
 * line. The month is not zero-padded; that keeps the string valid semver.
 *
 *   - YEAR/MONTH are the month of the commit being built — its committer date,
 *     in the committer's own timezone. Nobody bumps them: the line moves the
 *     moment a commit lands in a new month, and a rebuild of an old commit is
 *     still named what it shipped as, because a commit's date does not change.
 *     Not the build clock, which would move the version without a commit.
 *   - PATCH comes from `git rev-list --count HEAD`, which only exists at build
 *     time: the Go binary gets both stamped in via -ldflags, the web bundle gets
 *     the string inlined by Vite. Both call this file, so they can never
 *     disagree.
 *
 * Usage:
 *   node scripts/version.mjs            # print e.g. v2026.10.311
 *   node scripts/version.mjs --patch    # print just the commit count (311)
 *   node scripts/version.mjs --date     # print the commit's month (2026-10), for version.Date
 *   import { appVersion } from './scripts/version.mjs'
 */
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');

/**
 * The month of HEAD's commit as "YYYY-MM" — the value version.Date is stamped
 * with — or, with no commit to ask (a tarball, no git), the month of the build
 * clock, which is what the Go package falls back to as well, so an unstamped
 * build and a stamped one of the same tree agree.
 *
 * The committer date rather than the author's: a change authored in September
 * and landed in October is an October release. In the committer's own timezone
 * rather than UTC, so the month is the one on the commit as git shows it.
 */
export function commitMonth() {
  const iso = git(['log', '-1', '--format=%cI']);
  const m = iso && /^(\d{4})-(\d{2})/.exec(iso);
  if (m) {
    return `${m[1]}-${m[2]}`;
  }
  const t = new Date();
  return `${t.getUTCFullYear()}-${String(t.getUTCMonth() + 1).padStart(2, '0')}`;
}

/** Year and month as numbers, the way the version string wants them. */
function yearMonth() {
  const [year, month] = commitMonth().split('-').map(Number);
  if (!(month >= 1 && month <= 12)) {
    throw new Error(`commit month ${commitMonth()} is not a calendar month`);
  }
  return { year, month };
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
 * `2026.8.1`. Refuse it. Patch 0 is the agreed "unstamped build" marker (it
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
 * (v2026.8.0). Must stay byte-identical to version.String() in the Go package.
 *
 * Note `--patch` / commitCount() and `--date` / commitMonth() stay bare: those
 * feed `-ldflags -X` as the values of `version.Patch` and `version.Date`.
 */
export function appVersion() {
  const { year, month } = yearMonth();
  return `v${year}.${month}.${commitCount()}`;
}

// Invoked directly (by the build scripts), print rather than export.
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.stdout.write(
    process.argv.includes('--patch') ? commitCount()
      : process.argv.includes('--date') ? commitMonth()
        : appVersion(),
  );
  process.stdout.write('\n');
}
