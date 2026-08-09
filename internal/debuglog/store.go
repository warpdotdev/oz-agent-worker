package debuglog

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const (
	// DefaultMaxTotalBytes is the process-local disk budget shared by direct
	// captures and request snapshots.
	DefaultMaxTotalBytes int64 = 1 << 30
	// DefaultMaxExecutionBytes is the per-execution retention bound.
	DefaultMaxExecutionBytes = ProtocolCeilingBytes
	// DefaultMaxConcurrentUploads bounds how many snapshot/upload handlers run
	// at once per worker process.
	DefaultMaxConcurrentUploads = 2

	// captureDirName is the fixed sub-tree the worker owns beneath the
	// configured (or temporary) capture root.
	captureDirName = "oz-agent-worker/debug-logs"

	captureDirMode  os.FileMode = 0o700
	captureFileMode os.FileMode = 0o600
)

// Config bounds debug-log capture. It deliberately carries no retention
// duration: retention comes exclusively from the execution's already-resolved
// idle-on-complete cleanup grace.
type Config struct {
	// Directory overrides the capture root. Empty uses ${TMPDIR}.
	Directory string
	// MaxTotalBytes is the aggregate budget for captures plus request
	// snapshots in this process.
	MaxTotalBytes int64
	// MaxExecutionBytes bounds one execution's capture or snapshot.
	MaxExecutionBytes int64
	// MaxConcurrentUploads bounds concurrent snapshot/upload handlers.
	MaxConcurrentUploads int
}

// DefaultConfig returns the built-in bounds.
func DefaultConfig() Config {
	return Config{
		MaxTotalBytes:        DefaultMaxTotalBytes,
		MaxExecutionBytes:    DefaultMaxExecutionBytes,
		MaxConcurrentUploads: DefaultMaxConcurrentUploads,
	}
}

// WithDefaults fills unset bounds from DefaultConfig. Only a bound the operator
// actually set can fail validation, so an embedder that leaves the whole
// configuration zero still gets working capture.
func (c Config) WithDefaults() Config {
	defaults := DefaultConfig()
	if c.MaxTotalBytes == 0 {
		c.MaxTotalBytes = defaults.MaxTotalBytes
	}
	if c.MaxExecutionBytes == 0 {
		c.MaxExecutionBytes = defaults.MaxExecutionBytes
	}
	if c.MaxConcurrentUploads == 0 {
		c.MaxConcurrentUploads = defaults.MaxConcurrentUploads
	}
	return c
}

// Validate rejects bounds that cannot produce a usable capture. Callers treat
// a validation failure as a non-fatal loss of archive capture, never as a
// reason to refuse assigned task execution.
func (c Config) Validate() error {
	if c.MaxExecutionBytes <= 0 {
		return fmt.Errorf("debuglog: max_execution_bytes must be positive")
	}
	if c.MaxExecutionBytes > ProtocolCeilingBytes {
		return fmt.Errorf("debuglog: max_execution_bytes must not exceed %d", ProtocolCeilingBytes)
	}
	if c.MaxTotalBytes <= 0 {
		return fmt.Errorf("debuglog: max_total_bytes must be positive")
	}
	if c.MaxTotalBytes < c.MaxExecutionBytes {
		return fmt.Errorf("debuglog: max_total_bytes must be at least max_execution_bytes")
	}
	if c.MaxConcurrentUploads <= 0 {
		return fmt.Errorf("debuglog: max_concurrent_uploads must be positive")
	}
	return nil
}

// Store owns the secure capture root and the process-local disk budget. It is
// the only component that creates files under that root.
type Store struct {
	config Config
	root   string

	reserved atomic.Int64

	mu        sync.Mutex
	nextIndex uint64
}

// NewStore validates the configuration, prepares the capture root, and removes
// files orphaned by a previous process. V1 does not reconstruct ownership
// across process replacement, so anything already present is unowned garbage.
func NewStore(config Config) (*Store, error) {
	config = config.WithDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}

	base := config.Directory
	if base == "" {
		base = os.TempDir()
	}
	root := filepath.Join(base, captureDirName)

	if err := os.MkdirAll(root, captureDirMode); err != nil {
		return nil, fmt.Errorf("debuglog: failed to create capture root: %w", err)
	}
	// MkdirAll honors the process umask and leaves an existing directory's
	// mode alone, so tighten the leaf explicitly.
	if err := os.Chmod(root, captureDirMode); err != nil {
		return nil, fmt.Errorf("debuglog: failed to secure capture root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("debuglog: failed to inspect capture root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("debuglog: capture root is not a regular directory")
	}

	store := &Store{config: config, root: root}
	if err := store.removeOrphans(); err != nil {
		return nil, err
	}
	return store, nil
}

// Config returns the validated bounds this store enforces.
func (s *Store) Config() Config { return s.config }

// removeOrphans deletes every entry left in the capture root. Contents are
// never read, so nothing from a previous process is exposed.
func (s *Store) removeOrphans() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("debuglog: failed to scan capture root: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(s.root, entry.Name())); err != nil {
			return fmt.Errorf("debuglog: failed to remove orphaned capture data: %w", err)
		}
	}
	return nil
}

// ErrBudgetExhausted reports that the process-local disk budget cannot admit
// another capture or snapshot.
var ErrBudgetExhausted = fmt.Errorf("debuglog: capture disk budget exhausted")

// reserve claims bytes against the shared budget, or reports
// ErrBudgetExhausted when the budget is already committed.
func (s *Store) reserve(bytes int64) error {
	for {
		current := s.reserved.Load()
		if current+bytes > s.config.MaxTotalBytes {
			return ErrBudgetExhausted
		}
		if s.reserved.CompareAndSwap(current, current+bytes) {
			return nil
		}
	}
}

func (s *Store) release(bytes int64) {
	s.reserved.Add(-bytes)
}

// ReservedBytes reports the currently committed share of the disk budget.
func (s *Store) ReservedBytes() int64 { return s.reserved.Load() }

// createFile allocates one file with a non-user-derived name under the capture
// root. Opening with O_EXCL rejects a pre-existing entry, including a symlink
// planted to redirect writes outside the root.
func (s *Store) createFile(prefix, suffix string) (*os.File, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.nextIndex++
	index := s.nextIndex
	s.mu.Unlock()

	name := fmt.Sprintf("%s-%s-%d-%s", prefix, token, index, suffix)
	if filepath.Base(name) != name {
		return nil, fmt.Errorf("debuglog: refusing unsafe capture file name")
	}

	path := filepath.Join(s.root, name)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, captureFileMode) // #nosec G304 -- path is a random name the store generates beneath its own 0700 root.
	if err != nil {
		return nil, fmt.Errorf("debuglog: failed to create capture file: %w", err)
	}
	if err := file.Chmod(captureFileMode); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("debuglog: failed to secure capture file: %w", err)
	}
	return file, nil
}

func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("debuglog: failed to generate capture file name: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
