//go:build unix

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chinmay28/sand-vault/internal/provider"
)

// The sweep finishes when a disk does not. One Local folder account on a
// drive that has stopped answering used to park its ping in the kernel for
// good, and the sweep — which waits for every ping — with it: no scheduled
// check ever finished again, "Check now" never came back, and the account was
// never reported as anything. Now it is reported as a disk that is not
// answering, at the ping deadline, and the other accounts are reported as
// what they are.
func TestASweepFinishesWhenADiskDoesNotAnswer(t *testing.T) {
	v, roots := newTestVault(t, 2)
	// Connecting the accounts queued a backup push; let it land before the
	// folder is made to hang, or it is the push that hangs.
	v.AwaitBackupSync()

	// A FIFO where the write probe goes: open(2) for writing waits on it
	// until something opens it for reading, which is what a write to a dead
	// mount looks like from here. The cleanup is that reader.
	probe := filepath.Join(roots[0], ".sand-write-probe")
	if err := syscall.Mkfifo(probe, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		if r, err := os.OpenFile(probe, os.O_RDONLY, 0); err == nil {
			r.Close()
		}
	})

	was := healthPingTimeout
	healthPingTimeout = 300 * time.Millisecond
	t.Cleanup(func() { healthPingTimeout = was })

	started := time.Now()
	report, err := v.CheckClouds(t.Context())
	if err != nil {
		t.Fatalf("CheckClouds: %v", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the sweep waited %v on a disk that was not answering", took)
	}
	if report.Healthy != 1 || report.Unhealthy != 1 {
		t.Fatalf("got %d healthy, %d unhealthy; want 1 and 1: %+v", report.Healthy, report.Unhealthy, report.Clouds)
	}
	for _, c := range report.Clouds {
		if c.Healthy {
			continue
		}
		if !strings.Contains(c.Error, provider.ErrDiskNotAnswering.Error()) {
			t.Errorf("the hung account's error is %q, which does not say the disk is not answering", c.Error)
		}
	}
}
