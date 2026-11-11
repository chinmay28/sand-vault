package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync/atomic"
)

// splitter writes one stream out as part files of at most max bytes each.
//
// A part is committed only once it is known whether another follows it,
// because that is what its name says: the part being written is held open
// when it fills, and is closed as an ordinary part when the next byte
// arrives, or as the last when the stream is closed instead.
type splitter struct {
	ctx    context.Context
	store  Store
	base   string
	header partHeader
	max    int64 // 0 is no limit: one part, however big

	cur     Part
	curSize int64
	part    int // number of the part being written, from 1

	names []string // parts committed, in order
	bytes int64    // everything written, headers included

	// What a progress bar reads, kept apart from the two above because it is
	// read from another goroutine: the compressor writes down here from one
	// of its own, while whoever is feeding it asks how far things have got.
	started atomic.Int64 // parts started
	written atomic.Int64 // bytes written
}

func (s *splitter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if err := s.ctx.Err(); err != nil {
			return written, err
		}
		if s.cur != nil && s.max > 0 && s.curSize >= s.max {
			if err := s.commit(false); err != nil {
				return written, err
			}
		}
		if s.cur == nil {
			if err := s.open(); err != nil {
				return written, err
			}
		}

		chunk := p
		if s.max > 0 && int64(len(chunk)) > s.max-s.curSize {
			chunk = chunk[:s.max-s.curSize]
		}
		n, err := s.cur.Write(chunk)
		s.curSize += int64(n)
		s.count(n)
		written += n
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// count tallies bytes that reached a part.
func (s *splitter) count(n int) {
	s.bytes += int64(n)
	s.written.Store(s.bytes)
}

// open begins the next part with its header.
func (s *splitter) open() error {
	part, err := s.store.Create(s.ctx)
	if err != nil {
		return fmt.Errorf("starting part %d: %w", s.part+1, err)
	}
	s.part++
	s.started.Store(int64(s.part))
	s.cur, s.curSize = part, 0

	header := s.header
	header.part = uint32(s.part)
	n, err := part.Write(header.marshal())
	s.curSize += int64(n)
	s.count(n)
	if err != nil {
		return fmt.Errorf("writing part %d: %w", s.part, err)
	}
	return nil
}

// commit closes the part being written under the name its place gives it.
func (s *splitter) commit(last bool) error {
	name := partName(s.base, s.part, last)
	part := s.cur
	s.cur = nil
	if err := part.Commit(name); err != nil {
		part.Abort()
		return fmt.Errorf("storing %s: %w", name, err)
	}
	s.names = append(s.names, name)
	return nil
}

// Close commits the last part. A stream nothing was written to still leaves
// one part: its header.
func (s *splitter) Close() error {
	if s.cur == nil {
		if err := s.open(); err != nil {
			return err
		}
	}
	return s.commit(true)
}

// abort discards the part being written and erases the ones already stored.
// It takes a context of its own, since the one it was writing under is very
// likely the reason it is being called.
func (s *splitter) abort(ctx context.Context) {
	if s.cur != nil {
		s.cur.Abort()
		s.cur = nil
	}
	for _, name := range s.names {
		s.store.Remove(ctx, name)
	}
	s.names = nil
}

// joiner reads a snapshot's parts back as the one stream they were cut from,
// checking as each is opened that it is the part it should be.
type joiner struct {
	ctx   context.Context
	store Store
	names []string

	first partHeader // the first part's header, which every other must agree with
	next  int        // index into names of the part to open next
	cur   io.ReadCloser
}

// openJoiner opens the first part, which is where the key's salt and costs
// are read from.
func openJoiner(ctx context.Context, store Store, names []string) (*joiner, error) {
	if len(names) == 0 {
		return nil, ErrIncomplete
	}
	j := &joiner{ctx: ctx, store: store, names: names}
	if err := j.advance(); err != nil {
		return nil, err
	}
	return j, nil
}

// advance closes the part being read and opens the one after it.
func (j *joiner) advance() error {
	if j.cur != nil {
		j.cur.Close()
		j.cur = nil
	}
	name := j.names[j.next]
	body, err := j.store.Open(j.ctx, name)
	if err != nil {
		return fmt.Errorf("opening %s: %w", name, err)
	}
	header, err := readHeader(body)
	if err != nil {
		body.Close()
		return fmt.Errorf("%s: %w", name, err)
	}

	want := uint32(j.next + 1)
	switch {
	case header.part != want:
		body.Close()
		return fmt.Errorf("%w: %s is part %d, where part %d belongs", ErrDamaged, name, header.part, want)
	case j.next == 0:
		j.first = header
	case header.id != j.first.id || !bytes.Equal(header.salt[:], j.first.salt[:]):
		body.Close()
		return fmt.Errorf("%w: %s is a part of a different snapshot", ErrDamaged, name)
	}

	j.cur = body
	j.next++
	return nil
}

func (j *joiner) Read(p []byte) (int, error) {
	for {
		if err := j.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := j.cur.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == nil {
			continue
		}
		if err != io.EOF {
			return 0, fmt.Errorf("reading %s: %w", j.names[j.next-1], err)
		}
		if j.next == len(j.names) {
			return 0, io.EOF
		}
		if err := j.advance(); err != nil {
			return 0, err
		}
	}
}

func (j *joiner) Close() error {
	if j.cur == nil {
		return nil
	}
	err := j.cur.Close()
	j.cur = nil
	return err
}
