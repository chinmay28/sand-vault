package version

import (
	"regexp"
	"testing"
	"time"
)

// The rendered form is a release tag, a filename in dist/ and a string the e2e
// suite matches on screen, so its shape is a contract: v, a four-digit year,
// the month, then the commit count. No leading zero on the month — that is what
// keeps the tag valid semver.
func TestStringIsCalendarVersioned(t *testing.T) {
	want := regexp.MustCompile(`^v\d{4}\.([1-9]|1[0-2])\.\d+$`)
	if got := String(); !want.MatchString(got) {
		t.Errorf("String() = %q, want vYEAR.MONTH.PATCH with an unpadded month", got)
	}
}

func TestMonthIsACalendarMonth(t *testing.T) {
	if m := Month(); m < 1 || m > 12 {
		t.Errorf("Month() = %d, want a calendar month (1–12)", m)
	}
}

// A stamped build is named after its commit's month, whatever the clock says
// at build time — that is what lets an old tree rebuild as what it shipped as.
func TestAStampedBuildTakesItsMonthFromTheCommit(t *testing.T) {
	defer stamp(t, "2026-10", "165", time.Date(2027, 3, 9, 12, 0, 0, 0, time.UTC))()

	if Year() != 2026 || Month() != 10 {
		t.Errorf("Year, Month = %d, %d; want 2026, 10 from the stamp", Year(), Month())
	}
	if got := String(); got != "v2026.10.165" {
		t.Errorf("String() = %q, want v2026.10.165", got)
	}
}

// An unstamped build has no commit to ask, so it names the month it was built
// in — under patch 0, which marks it as a development build either way.
func TestAnUnstampedBuildTakesItsMonthFromTheClock(t *testing.T) {
	defer stamp(t, "", "0", time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC))()

	if got := String(); got != "v2026.11.0" {
		t.Errorf("String() = %q, want v2026.11.0 from the clock", got)
	}
}

// A stamp that is not a month is treated as no stamp rather than trusted or
// crashed on: the build still has a version.
func TestAMalformedStampFallsBackToTheClock(t *testing.T) {
	defer stamp(t, "october", "7", time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC))()

	if got := String(); got != "v2026.12.7" {
		t.Errorf("String() = %q, want v2026.12.7", got)
	}
}

// stamp sets the link-time values and the clock for one test, and hands back
// the restore.
func stamp(t *testing.T, date, patch string, at time.Time) func() {
	t.Helper()
	prevDate, prevPatch, prevNow := Date, Patch, now
	Date, Patch, now = date, patch, func() time.Time { return at }
	return func() { Date, Patch, now = prevDate, prevPatch, prevNow }
}
