// Package sandfile implements the on-disk format of a single encrypted part.
//
// A part is self-describing: everything needed to derive its key and open it
// is in the clear at the front of the file, and everything that would describe
// the file it came from is inside the ciphertext. Two of a file's three parts
// plus the right secret are enough to rebuild the original, with no index, no
// vault and no network involved.
package sandfile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// Magic bytes identifying a SAND part file.
var Magic = [4]byte{'S', 'A', 'N', 'D'}

// FormatVersion is the version this build writes.
//
// Version 2 moved the file's metadata — its name, its plaintext hash, and its
// sizes — out of the cleartext header and into the encrypted payload. In
// version 1 those sat in the clear, so anyone holding a single part could read
// the name and size of the file it belonged to without knowing any secret.
// Version 1 parts are still read, so archives written by older builds continue
// to restore.
const FormatVersion = 2

// LegacyFormatVersion is the older layout, still readable but never written.
const LegacyFormatVersion = 1

// MaxFilenameLength is the maximum allowed original filename length.
const MaxFilenameLength = 512

// saltLength is the Argon2id salt size carried by every part.
const saltLength = 16

// Header is the cleartext preamble of a part: the minimum a reader needs to
// derive the key and open the payload. It deliberately says nothing about the
// file the part came from beyond its size on disk.
//
// The archive ID is random and shared by a file's three parts, which is what
// lets a caller group parts that belong together without decrypting them.
type Header struct {
	Version    uint8
	PartNumber uint8    // 1, 2, or 3
	ArchiveID  [16]byte // random per archived file

	// Argon2id parameters
	Salt          []byte // 16 bytes
	Argon2Time    uint32
	Argon2Memory  uint32
	Argon2Threads uint8

	// AES-GCM nonce
	Nonce []byte // 12 bytes
}

// Metadata describes the archived file. From version 2 on it travels inside
// the encrypted payload, so it is readable only by someone who can already
// decrypt the part's contents.
type Metadata struct {
	Filename       string
	OriginalHash   [32]byte // SHA-256 of the original file
	OriginalSize   uint64
	CompressedSize uint64
	WasPadded      bool
}

// Part is a parsed part file, still encrypted.
type Part struct {
	Header *Header

	// AAD is the exact cleartext header as it appeared on disk, passed to
	// AES-GCM as associated data so a part cannot be re-labelled.
	AAD []byte

	// Ciphertext is the sealed payload: metadata followed by part data in
	// version 2, part data alone in version 1.
	Ciphertext []byte

	// legacyMetadata is the metadata read from a version 1 cleartext header.
	legacyMetadata *Metadata
}

// MarshalHeader serializes the cleartext header. The result is also the
// associated data for the payload's AES-GCM tag.
func MarshalHeader(h *Header) ([]byte, error) {
	if h.Version != FormatVersion {
		return nil, fmt.Errorf("cannot write format version %d, this build writes %d",
			h.Version, FormatVersion)
	}
	if h.PartNumber < 1 || h.PartNumber > 3 {
		return nil, fmt.Errorf("invalid part number: %d", h.PartNumber)
	}
	if len(h.Salt) != saltLength {
		return nil, fmt.Errorf("salt must be %d bytes, got %d", saltLength, len(h.Salt))
	}
	if len(h.Nonce) != crypto.NonceSize {
		return nil, fmt.Errorf("nonce must be %d bytes, got %d", crypto.NonceSize, len(h.Nonce))
	}

	buf := new(bytes.Buffer)
	buf.Write(Magic[:])
	buf.WriteByte(h.Version)
	buf.WriteByte(h.PartNumber)
	buf.Write(h.ArchiveID[:])
	buf.Write(h.Salt)
	binary.Write(buf, binary.BigEndian, h.Argon2Time)
	binary.Write(buf, binary.BigEndian, h.Argon2Memory)
	buf.WriteByte(h.Argon2Threads)
	buf.Write(h.Nonce)
	return buf.Bytes(), nil
}

// MarshalMetadata serializes the metadata block that precedes the part data
// inside the encrypted payload.
func MarshalMetadata(m *Metadata) ([]byte, error) {
	if len(m.Filename) > MaxFilenameLength {
		return nil, fmt.Errorf("filename too long: %d > %d", len(m.Filename), MaxFilenameLength)
	}

	buf := new(bytes.Buffer)
	buf.Write(m.OriginalHash[:])
	binary.Write(buf, binary.BigEndian, m.OriginalSize)
	binary.Write(buf, binary.BigEndian, m.CompressedSize)
	if m.WasPadded {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}
	nameBytes := []byte(m.Filename)
	binary.Write(buf, binary.BigEndian, uint16(len(nameBytes)))
	buf.Write(nameBytes)
	return buf.Bytes(), nil
}

// unmarshalMetadata reads a metadata block and returns it with the bytes that
// follow, which are the part's own data.
func unmarshalMetadata(data []byte) (*Metadata, []byte, error) {
	r := bytes.NewReader(data)
	m := &Metadata{}

	if err := binary.Read(r, binary.BigEndian, &m.OriginalHash); err != nil {
		return nil, nil, fmt.Errorf("reading original hash: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &m.OriginalSize); err != nil {
		return nil, nil, fmt.Errorf("reading original size: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &m.CompressedSize); err != nil {
		return nil, nil, fmt.Errorf("reading compressed size: %w", err)
	}

	var padByte uint8
	if err := binary.Read(r, binary.BigEndian, &padByte); err != nil {
		return nil, nil, fmt.Errorf("reading padding flag: %w", err)
	}
	m.WasPadded = padByte == 1

	var nameLen uint16
	if err := binary.Read(r, binary.BigEndian, &nameLen); err != nil {
		return nil, nil, fmt.Errorf("reading filename length: %w", err)
	}
	if nameLen > MaxFilenameLength {
		return nil, nil, fmt.Errorf("filename length %d exceeds max %d", nameLen, MaxFilenameLength)
	}
	nameBytes := make([]byte, nameLen)
	if _, err := io.ReadFull(r, nameBytes); err != nil {
		return nil, nil, fmt.Errorf("reading filename: %w", err)
	}
	m.Filename = string(nameBytes)

	return m, data[len(data)-r.Len():], nil
}

// Seal builds a complete part file: cleartext header, payload length, then the
// metadata and part data encrypted together under key.
func Seal(h *Header, m *Metadata, partData, key []byte) ([]byte, error) {
	headerBytes, err := MarshalHeader(h)
	if err != nil {
		return nil, fmt.Errorf("marshaling header: %w", err)
	}
	metaBytes, err := MarshalMetadata(m)
	if err != nil {
		return nil, fmt.Errorf("marshaling metadata: %w", err)
	}

	plaintext := make([]byte, 0, len(metaBytes)+len(partData))
	plaintext = append(plaintext, metaBytes...)
	plaintext = append(plaintext, partData...)

	ciphertext, err := crypto.Encrypt(key, h.Nonce, plaintext, headerBytes)
	if err != nil {
		return nil, fmt.Errorf("encrypting part %d: %w", h.PartNumber, err)
	}

	buf := new(bytes.Buffer)
	buf.Write(headerBytes)
	binary.Write(buf, binary.BigEndian, uint32(len(ciphertext)))
	buf.Write(ciphertext)
	return buf.Bytes(), nil
}

// ReadPart parses a part file without decrypting it.
func ReadPart(data []byte) (*Part, error) {
	if len(data) < 5 {
		return nil, errors.New("data too short for a part file")
	}
	if [4]byte{data[0], data[1], data[2], data[3]} != Magic {
		return nil, fmt.Errorf("invalid magic: expected SAND, got %q", string(data[:4]))
	}

	switch version := data[4]; version {
	case FormatVersion:
		return readV2(data)
	case LegacyFormatVersion:
		return readV1(data)
	default:
		return nil, fmt.Errorf("unsupported version: %d", version)
	}
}

// readV2 parses the current layout, where the header carries only what is
// needed to open the payload.
func readV2(data []byte) (*Part, error) {
	r := bytes.NewReader(data)
	h := &Header{}

	var magic [4]byte
	binary.Read(r, binary.BigEndian, &magic)
	if err := binary.Read(r, binary.BigEndian, &h.Version); err != nil {
		return nil, fmt.Errorf("reading version: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.PartNumber); err != nil {
		return nil, fmt.Errorf("reading part number: %w", err)
	}
	if h.PartNumber < 1 || h.PartNumber > 3 {
		return nil, fmt.Errorf("invalid part number: %d", h.PartNumber)
	}
	if err := binary.Read(r, binary.BigEndian, &h.ArchiveID); err != nil {
		return nil, fmt.Errorf("reading archive ID: %w", err)
	}

	h.Salt = make([]byte, saltLength)
	if _, err := io.ReadFull(r, h.Salt); err != nil {
		return nil, fmt.Errorf("reading salt: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.Argon2Time); err != nil {
		return nil, fmt.Errorf("reading argon2 time: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.Argon2Memory); err != nil {
		return nil, fmt.Errorf("reading argon2 memory: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.Argon2Threads); err != nil {
		return nil, fmt.Errorf("reading argon2 threads: %w", err)
	}
	h.Nonce = make([]byte, crypto.NonceSize)
	if _, err := io.ReadFull(r, h.Nonce); err != nil {
		return nil, fmt.Errorf("reading nonce: %w", err)
	}

	headerLen := len(data) - r.Len()
	ciphertext, err := readPayload(r, data, headerLen)
	if err != nil {
		return nil, err
	}
	return &Part{Header: h, AAD: data[:headerLen], Ciphertext: ciphertext}, nil
}

// readV1 parses the original layout, whose header held the file's name, hash
// and sizes in the clear.
func readV1(data []byte) (*Part, error) {
	r := bytes.NewReader(data)
	h := &Header{}
	m := &Metadata{}

	var magic [4]byte
	binary.Read(r, binary.BigEndian, &magic)
	if err := binary.Read(r, binary.BigEndian, &h.Version); err != nil {
		return nil, fmt.Errorf("reading version: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.PartNumber); err != nil {
		return nil, fmt.Errorf("reading part number: %w", err)
	}
	if h.PartNumber < 1 || h.PartNumber > 3 {
		return nil, fmt.Errorf("invalid part number: %d", h.PartNumber)
	}
	if err := binary.Read(r, binary.BigEndian, &h.ArchiveID); err != nil {
		return nil, fmt.Errorf("reading archive ID: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &m.OriginalHash); err != nil {
		return nil, fmt.Errorf("reading original hash: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &m.OriginalSize); err != nil {
		return nil, fmt.Errorf("reading original size: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &m.CompressedSize); err != nil {
		return nil, fmt.Errorf("reading compressed size: %w", err)
	}

	var padByte uint8
	if err := binary.Read(r, binary.BigEndian, &padByte); err != nil {
		return nil, fmt.Errorf("reading padding flag: %w", err)
	}
	m.WasPadded = padByte == 1

	var fnLen uint16
	if err := binary.Read(r, binary.BigEndian, &fnLen); err != nil {
		return nil, fmt.Errorf("reading filename length: %w", err)
	}
	if fnLen > MaxFilenameLength {
		return nil, fmt.Errorf("filename length %d exceeds max %d", fnLen, MaxFilenameLength)
	}
	fnBytes := make([]byte, fnLen)
	if _, err := io.ReadFull(r, fnBytes); err != nil {
		return nil, fmt.Errorf("reading filename: %w", err)
	}
	m.Filename = string(fnBytes)

	h.Salt = make([]byte, saltLength)
	if _, err := io.ReadFull(r, h.Salt); err != nil {
		return nil, fmt.Errorf("reading salt: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.Argon2Time); err != nil {
		return nil, fmt.Errorf("reading argon2 time: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.Argon2Memory); err != nil {
		return nil, fmt.Errorf("reading argon2 memory: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.Argon2Threads); err != nil {
		return nil, fmt.Errorf("reading argon2 threads: %w", err)
	}
	h.Nonce = make([]byte, crypto.NonceSize)
	if _, err := io.ReadFull(r, h.Nonce); err != nil {
		return nil, fmt.Errorf("reading nonce: %w", err)
	}

	headerLen := len(data) - r.Len()
	ciphertext, err := readPayload(r, data, headerLen)
	if err != nil {
		return nil, err
	}
	return &Part{Header: h, AAD: data[:headerLen], Ciphertext: ciphertext, legacyMetadata: m}, nil
}

// readPayload reads the 4-byte length prefix and the ciphertext behind it.
func readPayload(r *bytes.Reader, data []byte, headerLen int) ([]byte, error) {
	remaining := data[headerLen:]
	if len(remaining) < 4 {
		return nil, errors.New("file too short: missing payload size")
	}
	payloadSize := binary.BigEndian.Uint32(remaining[:4])
	remaining = remaining[4:]
	if uint32(len(remaining)) < payloadSize {
		return nil, fmt.Errorf("file truncated: expected %d payload bytes, got %d",
			payloadSize, len(remaining))
	}
	return remaining[:payloadSize], nil
}

// Open decrypts a part and returns the file's metadata together with the part's
// own data. It reads both format versions: version 1 carried its metadata in
// the clear, version 2 carries it inside the ciphertext.
func (p *Part) Open(key []byte) (*Metadata, []byte, error) {
	plaintext, err := crypto.Decrypt(key, p.Header.Nonce, p.Ciphertext, p.AAD)
	if err != nil {
		return nil, nil, err
	}
	// gcm.Open returns nil rather than an empty slice for empty plaintext;
	// normalize so callers checking for a present-but-empty part see one.
	if plaintext == nil {
		plaintext = []byte{}
	}

	if p.Header.Version == LegacyFormatVersion {
		return p.legacyMetadata, plaintext, nil
	}

	meta, partData, err := unmarshalMetadata(plaintext)
	if err != nil {
		return nil, nil, fmt.Errorf("reading part metadata: %w", err)
	}
	return meta, partData, nil
}
