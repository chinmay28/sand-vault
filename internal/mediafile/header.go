package mediafile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/chinmay28/sand-vault/internal/crypto"
)

// Magic bytes identifying a SAND media file.
var Magic = [4]byte{'S', 'A', 'N', 'D'}

// FormatVersion is the current media file format version.
const FormatVersion = 1

// MaxFilenameLength is the maximum allowed original filename length.
const MaxFilenameLength = 512

// Header contains all metadata for a SAND media file.
type Header struct {
	Version        uint8
	PartNumber     uint8    // 1, 2, or 3
	ArchiveID      [16]byte // UUID
	OriginalHash   [32]byte // SHA-256 of original file
	OriginalSize   uint64
	CompressedSize uint64
	WasPadded      bool
	Filename       string

	// Argon2id parameters
	Salt          []byte // 16 bytes
	Argon2Time    uint32
	Argon2Memory  uint32
	Argon2Threads uint8

	// AES-GCM nonce
	Nonce []byte // 12 bytes
}

// MarshalHeader serializes a Header to bytes (everything except the payload).
// This is also used as the Associated Data for AES-GCM.
func MarshalHeader(h *Header) ([]byte, error) {
	if len(h.Filename) > MaxFilenameLength {
		return nil, fmt.Errorf("filename too long: %d > %d", len(h.Filename), MaxFilenameLength)
	}
	if h.PartNumber < 1 || h.PartNumber > 3 {
		return nil, fmt.Errorf("invalid part number: %d", h.PartNumber)
	}
	if len(h.Salt) != 16 {
		return nil, fmt.Errorf("salt must be 16 bytes, got %d", len(h.Salt))
	}
	if len(h.Nonce) != crypto.NonceSize {
		return nil, fmt.Errorf("nonce must be %d bytes, got %d", crypto.NonceSize, len(h.Nonce))
	}

	buf := new(bytes.Buffer)

	buf.Write(Magic[:])
	buf.WriteByte(h.Version)
	buf.WriteByte(h.PartNumber)
	buf.Write(h.ArchiveID[:])
	buf.Write(h.OriginalHash[:])
	binary.Write(buf, binary.BigEndian, h.OriginalSize)
	binary.Write(buf, binary.BigEndian, h.CompressedSize)

	if h.WasPadded {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}

	filenameBytes := []byte(h.Filename)
	binary.Write(buf, binary.BigEndian, uint16(len(filenameBytes)))
	buf.Write(filenameBytes)

	buf.Write(h.Salt)
	binary.Write(buf, binary.BigEndian, h.Argon2Time)
	binary.Write(buf, binary.BigEndian, h.Argon2Memory)
	buf.WriteByte(h.Argon2Threads)
	buf.Write(h.Nonce)

	return buf.Bytes(), nil
}

// UnmarshalHeader deserializes a Header from bytes.
// Returns the header and the number of bytes consumed.
func UnmarshalHeader(data []byte) (*Header, int, error) {
	if len(data) < 4 {
		return nil, 0, errors.New("data too short for magic bytes")
	}

	r := bytes.NewReader(data)
	h := &Header{}

	var magic [4]byte
	if err := binary.Read(r, binary.BigEndian, &magic); err != nil {
		return nil, 0, fmt.Errorf("reading magic: %w", err)
	}
	if magic != Magic {
		return nil, 0, fmt.Errorf("invalid magic: expected SAND, got %s", string(magic[:]))
	}

	if err := binary.Read(r, binary.BigEndian, &h.Version); err != nil {
		return nil, 0, fmt.Errorf("reading version: %w", err)
	}
	if h.Version != FormatVersion {
		return nil, 0, fmt.Errorf("unsupported version: %d", h.Version)
	}

	if err := binary.Read(r, binary.BigEndian, &h.PartNumber); err != nil {
		return nil, 0, fmt.Errorf("reading part number: %w", err)
	}
	if h.PartNumber < 1 || h.PartNumber > 3 {
		return nil, 0, fmt.Errorf("invalid part number: %d", h.PartNumber)
	}

	if err := binary.Read(r, binary.BigEndian, &h.ArchiveID); err != nil {
		return nil, 0, fmt.Errorf("reading archive ID: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.OriginalHash); err != nil {
		return nil, 0, fmt.Errorf("reading original hash: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.OriginalSize); err != nil {
		return nil, 0, fmt.Errorf("reading original size: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.CompressedSize); err != nil {
		return nil, 0, fmt.Errorf("reading compressed size: %w", err)
	}

	var padByte uint8
	if err := binary.Read(r, binary.BigEndian, &padByte); err != nil {
		return nil, 0, fmt.Errorf("reading padding flag: %w", err)
	}
	h.WasPadded = padByte == 1

	var fnLen uint16
	if err := binary.Read(r, binary.BigEndian, &fnLen); err != nil {
		return nil, 0, fmt.Errorf("reading filename length: %w", err)
	}
	if fnLen > MaxFilenameLength {
		return nil, 0, fmt.Errorf("filename length %d exceeds max %d", fnLen, MaxFilenameLength)
	}

	fnBytes := make([]byte, fnLen)
	if _, err := r.Read(fnBytes); err != nil {
		return nil, 0, fmt.Errorf("reading filename: %w", err)
	}
	h.Filename = string(fnBytes)

	h.Salt = make([]byte, 16)
	if _, err := r.Read(h.Salt); err != nil {
		return nil, 0, fmt.Errorf("reading salt: %w", err)
	}

	if err := binary.Read(r, binary.BigEndian, &h.Argon2Time); err != nil {
		return nil, 0, fmt.Errorf("reading argon2 time: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.Argon2Memory); err != nil {
		return nil, 0, fmt.Errorf("reading argon2 memory: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &h.Argon2Threads); err != nil {
		return nil, 0, fmt.Errorf("reading argon2 threads: %w", err)
	}

	h.Nonce = make([]byte, crypto.NonceSize)
	if _, err := r.Read(h.Nonce); err != nil {
		return nil, 0, fmt.Errorf("reading nonce: %w", err)
	}

	consumed := len(data) - r.Len()
	return h, consumed, nil
}

// WriteMediaFile writes a complete media file: header + payload size (4 bytes) + encrypted payload.
func WriteMediaFile(header *Header, encryptedPayload []byte) ([]byte, error) {
	headerBytes, err := MarshalHeader(header)
	if err != nil {
		return nil, fmt.Errorf("marshaling header: %w", err)
	}

	buf := new(bytes.Buffer)
	buf.Write(headerBytes)
	binary.Write(buf, binary.BigEndian, uint32(len(encryptedPayload)))
	buf.Write(encryptedPayload)

	return buf.Bytes(), nil
}

// ReadMediaFile reads a complete media file and returns the header,
// the header bytes (for use as GCM associated data), and the encrypted payload.
func ReadMediaFile(data []byte) (*Header, []byte, []byte, error) {
	header, headerLen, err := UnmarshalHeader(data)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reading header: %w", err)
	}

	headerBytes := data[:headerLen]
	remaining := data[headerLen:]

	if len(remaining) < 4 {
		return nil, nil, nil, errors.New("file too short: missing payload size")
	}

	payloadSize := binary.BigEndian.Uint32(remaining[:4])
	remaining = remaining[4:]

	if uint32(len(remaining)) < payloadSize {
		return nil, nil, nil, fmt.Errorf("file truncated: expected %d payload bytes, got %d", payloadSize, len(remaining))
	}

	encryptedPayload := remaining[:payloadSize]
	return header, headerBytes, encryptedPayload, nil
}
