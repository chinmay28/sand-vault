//go:build unix

package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hangWriteProbe makes the next write probe on root block the way a write to
// a dead mount blocks: a FIFO where the probe file goes, which open(2) for
// writing waits on until something opens it for reading. The cleanup is that
// reader, which lets the parked goroutine finish and remove the probe.
func hangWriteProbe(t *testing.T, root string) {
	t.Helper()
	probe := filepath.Join(root, ".sand-write-probe")
	if err := syscall.Mkfifo(probe, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		r, err := os.OpenFile(probe, os.O_RDONLY, 0)
		if err == nil {
			r.Close()
		}
	})
}

// A folder whose disk has stopped answering is reported as such at the
// deadline, rather than holding the caller for as long as the kernel holds the
// write. This is what a failing drive or a vanished NFS mount does, and before
// this the ping sat in the syscall forever — taking the health sweep, which
// waits for every ping, with it.
func TestLocalGivesUpOnADiskThatDoesNotAnswer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sand")
	p := localAt(t, root)
	hangWriteProbe(t, root)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := p.Ping(ctx)
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the ping waited %v on a disk that was not answering", took)
	}
	if !errors.Is(err, ErrDiskNotAnswering) {
		t.Fatalf("ping error = %v, want ErrDiskNotAnswering", err)
	}
	if got := err.Error(); !strings.Contains(got, root) || !strings.Contains(got, "umount") {
		t.Errorf("the error does not name the folder and what to do about it: %q", got)
	}
}

// A caller that stopped waiting of its own accord has not found a dead disk,
// and must not be told it has.
func TestLocalCancellationIsNotADeadDisk(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sand")
	p := localAt(t, root)
	hangWriteProbe(t, root)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := p.Ping(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ping error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrDiskNotAnswering) {
		t.Error("a cancelled ping was reported as a disk that is not answering")
	}
}
