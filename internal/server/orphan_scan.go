package server

import (
	"time"

	"github.com/chinmay28/SandHarbor/internal/vault"
)

// The stray-parts scan as something that runs on its own, rather than inside
// the request that asked for it.
//
// GET /api/vault/orphans is one request that answers at the end, and for the
// banner over the file list that is the right shape: the app asks when the
// set of connected clouds changes and nobody is waiting on it. The settings
// panel is different. Somebody opened it to be told what is true now, the
// answer is a full listing of every account — minutes, on a slow cloud — and
// a page that is navigated away from cancels the request it was waiting on,
// so the minutes were spent for nothing and are spent again next time.
//
// So the panel's scan is started here, on a context of its own, and the page
// polls for it: where it has got to while it runs, and what it found once it
// is done. Closing the page keeps it; a page that comes back finds either the
// scan still going or the result waiting. One scan at a time — a second ask
// while one is running joins it, since the question is the same question —
// and the result is held until the next scan replaces it or the vault is
// locked, which takes the keys the result was read with and the file paths
// in it along with them.
//
// It lives in memory for as long as the process does and is never written
// down, for the reason the transfer watch gives: there is nothing worth
// writing. Running the scan again is how it resumes.

// strayScanTimeout bounds a listing of one account, the reattach that
// re-lists it, and the detached scan of every account — which gets the same
// ceiling, since it too is as slow as its slowest cloud. A listing that is cut
// off reads as "nothing there", which is the one wrong answer, so the bound is
// generous.
const strayScanTimeout = 10 * time.Minute

// scanWatch holds the scan that is running, or the last one that ran. The
// machinery is jobWatch's; a scan always asks about every account, so it is
// started under the empty scope and a second ask always joins.
type scanWatch = jobWatch[vault.OrphanScanProgress, vault.OrphanScan]

// scanStatus is what a poll is answered with — see jobStatus. What a finished
// scan found is under `result`.
type scanStatus = jobStatus[vault.OrphanScanProgress, vault.OrphanScan]

func newScanWatch() *scanWatch {
	return newJobWatch[vault.OrphanScanProgress, vault.OrphanScan]()
}
