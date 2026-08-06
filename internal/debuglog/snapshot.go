package debuglog

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

// Snapshot is one request's immutable, transformed NDJSON object on local
// disk. Keeping it on disk is what makes an upload retry byte-identical; it
// never extends an execution's retention and is deleted once the request
// finishes.
type Snapshot struct {
	store    *Store
	encoder  *Encoder
	file     *os.File
	reserved int64

	result    FinalizeResult
	crc32c    string
	sha256    string
	closeOnce sync.Once
}

// NewSnapshot allocates a request-scoped snapshot bounded by maxBytes.
func (s *Store) NewSnapshot(backend string, transformer ContentTransformer, maxBytes int64) (*Snapshot, error) {
	if maxBytes <= 0 || maxBytes > s.config.MaxExecutionBytes {
		maxBytes = s.config.MaxExecutionBytes
	}
	// The reservation covers the bounded spool plus the finalized object,
	// which briefly coexist while Finalize streams one into the other.
	reserved := maxBytes * 2
	if err := s.reserve(reserved); err != nil {
		return nil, err
	}

	encoder, err := NewEncoder(EncoderOptions{
		Backend:     backend,
		Transformer: transformer,
		MaxBytes:    maxBytes,
		CreateFile: func(suffix string) (*os.File, error) {
			return s.createFile("snapshot", suffix)
		},
	})
	if err != nil {
		s.release(reserved)
		return nil, err
	}

	file, err := s.createFile("snapshot", "object")
	if err != nil {
		_ = encoder.Close()
		s.release(reserved)
		return nil, err
	}

	return &Snapshot{store: s, encoder: encoder, file: file, reserved: reserved}, nil
}

// Sink is the destination a backend writes provider output into.
func (s *Snapshot) Sink() Sink { return s.encoder }

// Finalize writes the bounded NDJSON object and computes its digests over the
// exact bytes that will be uploaded.
func (s *Snapshot) Finalize() error {
	result, err := s.encoder.Finalize(s.file)
	if err != nil {
		return err
	}
	s.result = result

	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("debuglog: failed to flush snapshot: %w", err)
	}
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("debuglog: failed to stat snapshot: %w", err)
	}
	if info.Size() != result.Bytes {
		return fmt.Errorf("debuglog: snapshot size mismatch")
	}

	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("debuglog: failed to rewind snapshot: %w", err)
	}
	crcHash := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	shaHash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(crcHash, shaHash), s.file); err != nil {
		return fmt.Errorf("debuglog: failed to checksum snapshot: %w", err)
	}

	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crcHash.Sum32())
	s.crc32c = base64.StdEncoding.EncodeToString(crcBytes)
	s.sha256 = hex.EncodeToString(shaHash.Sum(nil))
	return nil
}

// Bytes is the finalized object's exact size.
func (s *Snapshot) Bytes() int64 { return s.result.Bytes }

// Truncated reports whether the object carries a truncation record.
func (s *Snapshot) Truncated() bool { return s.result.Truncated }

// Warnings are the deduplicated warning codes collected while encoding.
func (s *Snapshot) Warnings() []string { return s.result.Warnings }

// CRC32C is the Castagnoli checksum, base64-encoded to match object-store
// attribute formats.
func (s *Snapshot) CRC32C() string { return s.crc32c }

// SHA256 is the hex-encoded digest of the finalized object.
func (s *Snapshot) SHA256() string { return s.sha256 }

// Open returns an independent reader over the finalized object so every upload
// attempt replays exactly the same bytes.
func (s *Snapshot) Open() io.Reader {
	return io.NewSectionReader(s.file, 0, s.result.Bytes)
}

// Close deletes the snapshot's bytes and returns its share of the disk budget.
func (s *Snapshot) Close() {
	s.closeOnce.Do(func() {
		_ = s.encoder.Close()
		_ = closeAndRemove(s.file)
		s.store.release(s.reserved)
	})
}
