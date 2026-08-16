// Package version carries the application version.
//
// The scheme is calendar-based: vYEAR.MONTH.PATCH, where the patch number is
// the repository's commit count — every commit is a patch release, so
// `v2026.8.311` is the 311th commit on the 2026.8 line. The month is written
// as a plain number, not zero-padded: that keeps the string valid semver, which
// forbids a leading zero, and nothing here orders versions by sorting text.
//
// Year and Month are declared here in source and bumped by hand when a release
// line opens — deliberately not read from the build clock, which would move the
// version without a commit and make a rebuild of an old tree disagree with what
// it originally shipped. The patch number can only come from git, which a
// compiled binary has no access to, so it is stamped at link time instead:
//
//	go build -ldflags "-X github.com/chinmay28/sand-vault/internal/version.Patch=$(git rev-list --count HEAD)"
//
// `make build` (and `build-go`) does this for you via scripts/version.mjs,
// which is also what the web client's build reads Year/Month from — keep the
// two constants below in a form that file's regex can still find.
package version

import "strconv"

// Year and Month of the release line, bumped by hand. Month is a calendar
// month, 1–12.
const (
	Year  = 2026
	Month = 8
)

// Patch is the repository's commit count, stamped at link time (see the
// package comment). A bare `go build` leaves it at "0": patch 0 means an
// unstamped development build, never a release.
var Patch = "0"

// String renders the full version, `v`-prefixed to match how the project tags
// releases (v2026.8.0). This is the one rendering — it's what the CLI prints,
// what /api/health reports, and what the web client shows in its header.
func String() string {
	return "v" + strconv.Itoa(Year) + "." + strconv.Itoa(Month) + "." + Patch
}
