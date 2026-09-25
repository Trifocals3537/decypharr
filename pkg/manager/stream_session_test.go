package manager

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type sessionRange struct {
	start int64
	end   int64
}

type sessionSource struct {
	mu    sync.Mutex
	data  []byte
	calls []sessionRange
	hook  func(context.Context, int64, int64, io.Writer) error
}

func (s *sessionSource) stream(ctx context.Context, start, end int64, writer io.Writer) error {
	s.mu.Lock()
	s.calls = append(s.calls, sessionRange{start: start, end: end})
	hook := s.hook
	data := s.data
	s.mu.Unlock()
	if hook != nil {
		return hook(ctx, start, end, writer)
	}
	_, err := writer.Write(data[start : end+1])
	return err
}

func (s *sessionSource) ranges() []sessionRange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sessionRange(nil), s.calls...)
}

func TestStreamSessionSequentialReadAndSeek(t *testing.T) {
	source := &sessionSource{data: []byte("abcdefghij")}
	session, err := newStreamSession(t.Context(), int64(len(source.data)), 0, source.stream)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	buf := make([]byte, 4)
	n, err := session.Read(buf)
	if err != nil || n != 4 || string(buf) != "abcd" {
		t.Fatalf("first read = %d, %q, %v", n, buf, err)
	}
	if got, err := session.Seek(2, io.SeekCurrent); err != nil || got != 6 {
		t.Fatalf("seek current = %d, %v", got, err)
	}

	buf = make([]byte, 3)
	n, err = session.Read(buf)
	if err != nil || n != 3 || string(buf) != "ghi" {
		t.Fatalf("second read = %d, %q, %v", n, buf, err)
	}
	if got, err := session.Seek(-2, io.SeekEnd); err != nil || got != 8 {
		t.Fatalf("seek end = %d, %v", got, err)
	}

	buf = make([]byte, 4)
	n, err = session.Read(buf)
	if err != nil || n != 2 || string(buf[:n]) != "ij" {
		t.Fatalf("tail read = %d, %q, %v", n, buf[:n], err)
	}
	if n, err = session.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF read = %d, %v", n, err)
	}

	want := []sessionRange{{0, 3}, {6, 8}, {8, 9}}
	got := source.ranges()
	if len(got) != len(want) {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestStreamSessionPrimeDoesNotAdvance(t *testing.T) {
	source := &sessionSource{data: []byte("prime")}
	session, err := newStreamSession(t.Context(), int64(len(source.data)), 0, source.stream)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if err := session.Prime(); err != nil {
		t.Fatal(err)
	}
	if err := session.Prime(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if n, err := session.Read(buf); err != nil || n != 2 || string(buf) != "pr" {
		t.Fatalf("read after prime = %d, %q, %v", n, buf, err)
	}

	want := []sessionRange{{0, 0}, {0, 1}}
	got := source.ranges()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ranges = %#v, want %#v", got, want)
	}
}

func TestStreamSessionRejectsIncompleteExactRange(t *testing.T) {
	source := &sessionSource{data: []byte("short")}
	source.hook = func(_ context.Context, _, _ int64, writer io.Writer) error {
		_, err := writer.Write([]byte("sh"))
		return err
	}
	session, err := newStreamSession(t.Context(), int64(len(source.data)), 0, source.stream)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	buf := make([]byte, 4)
	n, err := session.Read(buf)
	if n != 2 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short read = %d, %v", n, err)
	}
	if got, err := session.Seek(0, io.SeekCurrent); err != nil || got != 2 {
		t.Fatalf("position after short read = %d, %v", got, err)
	}
}

func TestStreamSessionCloseCancelsBlockedRead(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	source := &sessionSource{data: []byte("cancel")}
	source.hook = func(ctx context.Context, _, _ int64, _ io.Writer) error {
		startedOnce.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}
	session, err := newStreamSession(t.Context(), int64(len(source.data)), 0, source.stream)
	if err != nil {
		t.Fatal(err)
	}

	readDone := make(chan error, 1)
	go func() {
		_, readErr := session.Read(make([]byte, 1))
		readDone <- readErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case readErr := <-readDone:
		if !errors.Is(readErr, context.Canceled) {
			t.Fatalf("blocked read error = %v", readErr)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock read")
	}
	if _, err := session.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read after close = %v", err)
	}
}

func TestStreamSessionSeekValidation(t *testing.T) {
	session, err := newStreamSession(t.Context(), 10, 0, func(context.Context, int64, int64, io.Writer) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if _, err := session.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek succeeded")
	}
	if _, err := session.Seek(0, 99); err == nil {
		t.Fatal("invalid whence succeeded")
	}
	if _, err := session.Seek(math.MaxInt64, io.SeekEnd); err == nil {
		t.Fatal("overflowing seek succeeded")
	}
	if got, err := session.Seek(5, io.SeekEnd); err != nil || got != 15 {
		t.Fatalf("seek past EOF = %d, %v", got, err)
	}
	if n, err := session.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read past EOF = %d, %v", n, err)
	}
}

func TestOpenStreamTracksUntilClose(t *testing.T) {
	mgr := &Manager{activeStreams: xsync.NewMap[string, *ActiveStream]()}
	entry := &storage.Entry{
		Name: "movie",
		Files: map[string]*storage.File{
			"video.mkv": {Name: "video.mkv", Size: 1024},
		},
	}

	stream, err := mgr.OpenStream(t.Context(), entry, "video.mkv", 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if mgr.GetActiveStreamsCount() != 1 {
		t.Fatalf("active streams = %d, want 1", mgr.GetActiveStreamsCount())
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if mgr.GetActiveStreamsCount() != 0 {
		t.Fatalf("active streams after close = %d, want 0", mgr.GetActiveStreamsCount())
	}
}

func TestOpenStreamValidatesEntryAndOffset(t *testing.T) {
	mgr := &Manager{activeStreams: xsync.NewMap[string, *ActiveStream]()}
	entry := &storage.Entry{
		Name: "movie",
		Files: map[string]*storage.File{
			"video.mkv": {Name: "video.mkv", Size: 10},
			"empty.mkv": {Name: "empty.mkv"},
		},
	}

	tests := []struct {
		name     string
		entry    *storage.Entry
		filename string
		offset   int64
	}{
		{name: "nil entry", filename: "video.mkv"},
		{name: "missing file", entry: entry, filename: "missing.mkv"},
		{name: "empty file", entry: entry, filename: "empty.mkv"},
		{name: "negative offset", entry: entry, filename: "video.mkv", offset: -1},
		{name: "offset beyond size", entry: entry, filename: "video.mkv", offset: 11},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if stream, err := mgr.OpenStreamUntracked(t.Context(), tc.entry, tc.filename, tc.offset); err == nil {
				stream.Close()
				t.Fatal("invalid stream opened")
			}
		})
	}
}
