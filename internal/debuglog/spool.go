package debuglog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// ErrSpoolClosed is returned when a caller writes to a spool that has already
// been closed.
var ErrSpoolClosed = errors.New("debuglog: spool closed")

// boundedSpool stores newline-terminated lines on disk under a fixed byte
// budget using a first/last retention policy: the head segment keeps the
// earliest lines and two rotating tail segments keep the most recent ones.
// Retention is always whole lines, so a reader never sees a partial record.
//
// Head-and-tail retention keeps both the early context of a failing execution
// (image pull, setup) and its terminal output, at the cost of an explicit gap
// that the caller reports as a truncation record.
type boundedSpool struct {
	mu sync.Mutex

	head    *os.File
	tailOld *os.File
	tailNew *os.File

	headLimit int64
	tailLimit int64

	headBytes    int64
	tailOldBytes int64
	tailNewBytes int64
	omitted      int64

	closed bool
}

// spoolWatermark is a consistent view of a spool's contents at one instant.
// A reader replays exactly these byte prefixes while writers keep appending.
type spoolWatermark struct {
	headBytes    int64
	tailOldBytes int64
	tailNewBytes int64
	omitted      int64
}

// newBoundedSpool creates a spool bounded to maxBytes total retained bytes.
// Files are created through create so callers control mode and naming.
func newBoundedSpool(maxBytes int64, create func(suffix string) (*os.File, error)) (*boundedSpool, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("debuglog: spool budget must be positive, got %d", maxBytes)
	}

	head, err := create("head")
	if err != nil {
		return nil, err
	}
	tailOld, err := create("tail-a")
	if err != nil {
		_ = closeAndRemove(head)
		return nil, err
	}
	tailNew, err := create("tail-b")
	if err != nil {
		_ = closeAndRemove(head)
		_ = closeAndRemove(tailOld)
		return nil, err
	}

	// Half the budget preserves the earliest output; the remaining half is
	// split across two rotating tail segments so the newest output survives
	// while each rotation drops at most a quarter of the budget.
	headLimit := maxBytes / 2
	if headLimit <= 0 {
		headLimit = 1
	}
	tailLimit := (maxBytes - headLimit) / 2
	if tailLimit <= 0 {
		tailLimit = 1
	}

	return &boundedSpool{
		head:      head,
		tailOld:   tailOld,
		tailNew:   tailNew,
		headLimit: headLimit,
		tailLimit: tailLimit,
	}, nil
}

// WriteLine appends one complete line, rotating tail segments when the budget
// is exhausted. A line larger than a tail segment is retained in full to keep
// the stream parseable; the budget is a target, not a hard per-line cap, and
// callers bound line size with MaxChunkBytes.
func (s *boundedSpool) WriteLine(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrSpoolClosed
	}

	if s.headBytes+int64(len(line)) <= s.headLimit {
		if _, err := s.head.Write(line); err != nil {
			return err
		}
		s.headBytes += int64(len(line))
		return nil
	}

	if s.tailNewBytes > 0 && s.tailNewBytes+int64(len(line)) > s.tailLimit {
		if err := s.rotateTailLocked(); err != nil {
			return err
		}
	}
	if _, err := s.tailNew.Write(line); err != nil {
		return err
	}
	s.tailNewBytes += int64(len(line))
	return nil
}

// rotateTailLocked retires the older tail segment and reuses its file for new
// output. The retired bytes become the reported omission lower bound.
func (s *boundedSpool) rotateTailLocked() error {
	s.omitted += s.tailOldBytes

	retired := s.tailOld
	if err := retired.Truncate(0); err != nil {
		return err
	}
	if _, err := retired.Seek(0, io.SeekStart); err != nil {
		return err
	}

	s.tailOld = s.tailNew
	s.tailOldBytes = s.tailNewBytes
	s.tailNew = retired
	s.tailNewBytes = 0
	return nil
}

// Watermark captures the current retained extents so a snapshot reads a fixed
// prefix of each segment even while writers continue appending.
func (s *boundedSpool) Watermark() spoolWatermark {
	s.mu.Lock()
	defer s.mu.Unlock()
	return spoolWatermark{
		headBytes:    s.headBytes,
		tailOldBytes: s.tailOldBytes,
		tailNewBytes: s.tailNewBytes,
		omitted:      s.omitted,
	}
}

// segmentsAt returns readers over the exact byte extents recorded in mark, in
// chronological order. The returned readers are independent of the write
// offsets, so reading never disturbs a concurrent writer.
func (s *boundedSpool) segmentsAt(mark spoolWatermark) []io.Reader {
	s.mu.Lock()
	defer s.mu.Unlock()

	return []io.Reader{
		io.NewSectionReader(s.head, 0, mark.headBytes),
		io.NewSectionReader(s.tailOld, 0, mark.tailOldBytes),
		io.NewSectionReader(s.tailNew, 0, mark.tailNewBytes),
	}
}

// Close releases the spool's files and deletes them.
func (s *boundedSpool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	var errs []error
	for _, f := range []*os.File{s.head, s.tailOld, s.tailNew} {
		if err := closeAndRemove(f); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func closeAndRemove(f *os.File) error {
	if f == nil {
		return nil
	}
	name := f.Name()
	closeErr := f.Close()
	removeErr := os.Remove(name)
	if removeErr != nil && os.IsNotExist(removeErr) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}
