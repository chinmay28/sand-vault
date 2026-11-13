//go:build unix

package server

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedLog is somewhere for the log to go during a test that reads it back:
// the log package serializes its own writes, but nothing serializes them
// against the test reading the buffer.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// The thirty minutes a stream used to be allowed were there so that a hung
// provider could not pin its handler open forever, and the clock that took
// their place has to go on doing that: a film the vault stops feeding part
// way through is given up on once the vault has kept it waiting for the
// limit, however long it had been playing well before that.
//
// The disk stops answering the way a dead one does. Every part of one chunk
// in the middle of the film is made a FIFO, which open(2) for reading waits
// on until something opens it for writing, and nothing does until the test
// is over.
func TestAStreamTheVaultStopsFeedingIsGivenUp(t *testing.T) {
	c := newTestClient(t)
	roots := c.setup("stream-me-please", 3)
	c.smallChunks()
	stallAfter(t, 300*time.Millisecond)

	film := filmOf(512 << 10)
	file := c.upload("film.mkv", "/", film)
	link := c.mintStream(file["id"].(string))

	const hung = 8
	archive, _ := file["archive_id"].(string)
	var fifos []string
	for _, root := range roots {
		parts, err := filepath.Glob(filepath.Join(root, fmt.Sprintf("%s-c%07d-p*.sand", archive, hung)))
		if err != nil {
			t.Fatalf("finding chunk %d's parts: %v", hung, err)
		}
		for _, part := range parts {
			if err := os.Remove(part); err != nil {
				t.Fatalf("removing %s: %v", part, err)
			}
			if err := syscall.Mkfifo(part, 0o600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
			fifos = append(fifos, part)
		}
	}
	if len(fifos) != 3 {
		t.Fatalf("found %d parts of chunk %d, want all three", len(fifos), hung)
	}
	// The reads parked on them are let go at the end: a writer that opens the
	// FIFO and closes it again is an empty file to the reader waiting there.
	t.Cleanup(func() {
		for _, fifo := range fifos {
			if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				w.Close()
			}
		}
	})

	logged := &lockedLog{}
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	client, done := c.play(c.get(link))
	started := time.Now()
	client.drain(t, done)
	took := time.Since(started)

	got := client.received()
	if len(got) == 0 || len(got) > hung*32<<10 || !bytes.Equal(got, film[:len(got)]) {
		t.Errorf("the stream delivered %d bytes; want the film up to the chunk that hung, and no further", len(got))
	}
	if took > 5*time.Second {
		t.Errorf("the stream took %v to give up on a vault that had stopped answering", took)
	}
	if !strings.Contains(logged.String(), "stream of /film.mkv stopped") {
		t.Errorf("nothing in the log says why the film stopped; the log was:\n%s", logged.String())
	}
}
