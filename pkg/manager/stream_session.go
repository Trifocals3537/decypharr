package manager

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sync"

	"github.com/sirrobot01/decypharr/pkg/storage"
)

// StreamReader is a seekable, bounded view of one managed media file.
//
// The initial implementation deliberately delegates every read to Manager.Stream
// so callers retain the existing range validation, recovery, provider failover,
// and circuit-breaker behavior. It establishes the stable consumer contract
// needed to move those details behind persistent protocol sessions later.
type StreamReader interface {
	io.ReadCloser
	io.Seeker
	Size() int64
	Prime() error
}

type streamRangeFunc func(context.Context, int64, int64, io.Writer) error

type streamSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	stream streamRangeFunc
	size   int64

	mu       sync.Mutex
	pos      int64
	primedAt int64
	closed   bool
	onClose  func()
}

func newStreamSession(
	ctx context.Context,
	size int64,
	offset int64,
	stream streamRangeFunc,
) (*streamSession, error) {
	if size <= 0 {
		return nil, fmt.Errorf("invalid stream size %d", size)
	}
	if offset < 0 || offset > size {
		return nil, fmt.Errorf("stream offset %d is outside file size %d", offset, size)
	}
	if stream == nil {
		return nil, fmt.Errorf("stream range reader is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	return &streamSession{
		ctx:      sessionCtx,
		cancel:   cancel,
		stream:   stream,
		size:     size,
		pos:      offset,
		primedAt: -1,
	}, nil
}

func (s *streamSession) Size() int64 {
	return s.size
}

func (s *streamSession) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, os.ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if s.pos >= s.size {
		return 0, io.EOF
	}

	want := int64(len(p))
	if remaining := s.size - s.pos; want > remaining {
		want = remaining
	}
	start := s.pos
	writer := &streamSliceWriter{dst: p[:int(want)]}
	err := s.stream(s.ctx, start, start+want-1, writer)
	n := writer.written
	s.pos += int64(n)
	s.primedAt = -1

	if int64(n) == want {
		// A complete exact-range read satisfies this call even if an upstream
		// reader also reported EOF at that range boundary.
		if err == nil || err == io.EOF {
			return n, nil
		}
		return n, err
	}
	if err != nil {
		return n, err
	}
	if n == 0 {
		return 0, io.ErrNoProgress
	}
	return n, io.ErrUnexpectedEOF
}

// Prime verifies that the current byte can be opened without advancing the
// logical position. Repeated calls at the same position do not repeat I/O.
func (s *streamSession) Prime() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return os.ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.pos >= s.size || s.primedAt == s.pos {
		return nil
	}

	var probe [1]byte
	writer := &streamSliceWriter{dst: probe[:]}
	err := s.stream(s.ctx, s.pos, s.pos, writer)
	if writer.written == len(probe) && (err == nil || err == io.EOF) {
		s.primedAt = s.pos
		return nil
	}
	if err != nil {
		return err
	}
	if writer.written == 0 {
		return io.ErrNoProgress
	}
	return io.ErrUnexpectedEOF
}

func (s *streamSession) Seek(offset int64, whence int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, os.ErrClosed
	}

	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = s.pos
	case io.SeekEnd:
		base = s.size
	default:
		return 0, fmt.Errorf("invalid seek whence %d", whence)
	}

	next, ok := addStreamOffset(base, offset)
	if !ok {
		return 0, fmt.Errorf("stream seek overflows int64")
	}
	if next < 0 {
		return 0, fmt.Errorf("invalid seek to %d", next)
	}
	if next != s.pos {
		s.pos = next
		s.primedAt = -1
	}
	return next, nil
}

func (s *streamSession) Close() error {
	// Cancel first so a range operation holding s.mu can return promptly.
	s.cancel()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	onClose := s.onClose
	s.onClose = nil
	s.mu.Unlock()

	if onClose != nil {
		onClose()
	}
	return nil
}

func addStreamOffset(base, offset int64) (int64, bool) {
	if offset > 0 && base > math.MaxInt64-offset {
		return 0, false
	}
	if offset < 0 && base < math.MinInt64-offset {
		return 0, false
	}
	return base + offset, true
}

type streamSliceWriter struct {
	dst     []byte
	written int
}

func (w *streamSliceWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if w.written >= len(w.dst) {
		return 0, io.ErrShortWrite
	}
	n := copy(w.dst[w.written:], p)
	w.written += n
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

// OpenStream returns a tracked seekable session over one managed file.
// Connecting remains lazy; call Prime when a consumer must fail before it
// commits response headers.
func (m *Manager) OpenStream(
	ctx context.Context,
	entry *storage.Entry,
	filename string,
	offset int64,
	client string,
) (StreamReader, error) {
	session, err := m.openStreamUntracked(ctx, entry, filename, offset, client)
	if err != nil {
		return nil, err
	}
	streamID := m.TrackStream(entry, filename, client)
	session.onClose = func() { m.UntrackStream(streamID) }
	return session, nil
}

// OpenStreamUntracked returns a session for consumers that own aggregate
// stream accounting, such as the shared DFS downloader coordinator.
func (m *Manager) OpenStreamUntracked(
	ctx context.Context,
	entry *storage.Entry,
	filename string,
	offset int64,
) (StreamReader, error) {
	return m.openStreamUntracked(ctx, entry, filename, offset, "")
}

func (m *Manager) openStreamUntracked(
	ctx context.Context,
	entry *storage.Entry,
	filename string,
	offset int64,
	client string,
) (*streamSession, error) {
	if m == nil {
		return nil, fmt.Errorf("manager is nil")
	}
	if entry == nil {
		return nil, fmt.Errorf("stream entry is nil")
	}
	file, ok := entry.Files[filename]
	if !ok || file == nil {
		return nil, fmt.Errorf("file %s not found in entry %s", filename, entry.Name)
	}

	return newStreamSession(ctx, file.Size, offset, func(
		streamCtx context.Context,
		start int64,
		end int64,
		writer io.Writer,
	) error {
		return m.Stream(streamCtx, entry, filename, start, end, writer, nil, client)
	})
}
