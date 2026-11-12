package snapshot

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
)

// The sealed stream.
//
// A snapshot can be bigger than the machine making it, so it cannot be sealed
// in one go the way a shard is. It is sealed a frame at a time instead: each
// frame is up to frameSize bytes of the stream under AES-256-GCM, written as
// a four-byte length and then the ciphertext.
//
// Three things a stream of sealed frames can suffer that one sealed blob
// cannot, and what stops each:
//
//   - frames put back in a different order: the nonce is the frame's number,
//     so a frame only opens in the place it was sealed for;
//   - a frame from another snapshot spliced in: the snapshot's ID is bound to
//     every frame, and the key is the snapshot's own in any case;
//   - the stream cut short at a frame boundary: the last frame is sealed as
//     the last, and a stream that ends on any other frame has not ended.
//
// The key is used for one snapshot and never again — it comes from a salt
// drawn fresh for each — which is what makes a counter a safe nonce.

// frameSize is how much of the stream one frame holds. A megabyte: small
// enough that a damaged byte costs one frame's worth of reading before it is
// noticed, large enough that the sixteen bytes of tag per frame are nothing.
const frameSize = 1 << 20

// finalBit marks the last frame, in the top bit of its length. It is bound to
// the frame as well as written beside it, so flipping it in transit fails the
// frame rather than moving the end.
const finalBit = 1 << 31

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// frameNonce is the nonce of the nth frame.
func frameNonce(n uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], n)
	return nonce
}

// frameAAD is what every frame is bound to besides its place: the snapshot it
// belongs to, and whether it is the last.
func frameAAD(id [idSize]byte, final bool) []byte {
	aad := make([]byte, idSize+1)
	copy(aad, id[:])
	if final {
		aad[idSize] = 1
	}
	return aad
}

// sealer writes a stream out as sealed frames.
type sealer struct {
	w     io.Writer
	aead  cipher.AEAD
	id    [idSize]byte
	frame uint64
	buf   []byte
	done  bool
}

func newSealer(w io.Writer, key []byte, id [idSize]byte) (*sealer, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &sealer{w: w, aead: aead, id: id, buf: make([]byte, 0, frameSize)}, nil
}

func (s *sealer) Write(p []byte) (int, error) {
	if s.done {
		return 0, fmt.Errorf("write to a sealed stream that has been closed")
	}
	written := 0
	for len(p) > 0 {
		// A full frame is held until there is something to follow it, so
		// that the frame the stream ends on can be sealed as the last.
		if len(s.buf) == frameSize {
			if err := s.flush(false); err != nil {
				return written, err
			}
		}
		n := copy(s.buf[len(s.buf):frameSize], p)
		s.buf = s.buf[:len(s.buf)+n]
		p = p[n:]
		written += n
	}
	return written, nil
}

func (s *sealer) flush(final bool) error {
	sealed := s.aead.Seal(nil, frameNonce(s.frame), s.buf, frameAAD(s.id, final))
	length := uint32(len(sealed))
	if final {
		length |= finalBit
	}
	head := binary.BigEndian.AppendUint32(nil, length)
	if _, err := s.w.Write(head); err != nil {
		return err
	}
	if _, err := s.w.Write(sealed); err != nil {
		return err
	}
	s.frame++
	s.buf = s.buf[:0]
	return nil
}

// Close seals whatever is held as the last frame. A stream with nothing in it
// still gets one: the last frame is what says the stream is whole.
func (s *sealer) Close() error {
	if s.done {
		return nil
	}
	s.done = true
	return s.flush(true)
}

// opener reads sealed frames back as the stream they were.
type opener struct {
	r     io.Reader
	aead  cipher.AEAD
	id    [idSize]byte
	frame uint64
	buf   []byte // what is left of the frame being read
	done  bool
	err   error
}

func newOpener(r io.Reader, key []byte, id [idSize]byte) (*opener, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &opener{r: r, aead: aead, id: id}, nil
}

func (o *opener) Read(p []byte) (int, error) {
	for len(o.buf) == 0 {
		if o.err != nil {
			return 0, o.err
		}
		if o.done {
			return 0, io.EOF
		}
		if err := o.next(); err != nil {
			o.err = err
			return 0, err
		}
	}
	n := copy(p, o.buf)
	o.buf = o.buf[n:]
	return n, nil
}

// next reads and opens one frame.
func (o *opener) next() error {
	var head [4]byte
	if _, err := io.ReadFull(o.r, head[:]); err != nil {
		// The parts ran out before a frame said it was the last.
		return fmt.Errorf("%w: it ends before its last frame", ErrDamaged)
	}
	length := binary.BigEndian.Uint32(head[:])
	final := length&finalBit != 0
	length &^= finalBit
	if int(length) < o.aead.Overhead() || int(length) > frameSize+o.aead.Overhead() {
		return fmt.Errorf("%w: frame %d has a length no frame has", ErrDamaged, o.frame)
	}

	sealed := make([]byte, length)
	if _, err := io.ReadFull(o.r, sealed); err != nil {
		return fmt.Errorf("%w: it ends inside frame %d", ErrDamaged, o.frame)
	}
	plain, err := o.aead.Open(sealed[:0], frameNonce(o.frame), sealed, frameAAD(o.id, final))
	if err != nil {
		// The first frame is where a wrong password shows itself, and it
		// cannot be told from a damaged one. Any later frame opened under a
		// key that has already opened one is damage.
		if o.frame == 0 {
			return ErrPassword
		}
		return fmt.Errorf("%w: frame %d does not open", ErrDamaged, o.frame)
	}
	o.frame++
	o.buf = plain
	o.done = final
	return nil
}
