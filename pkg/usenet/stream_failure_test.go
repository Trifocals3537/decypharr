package usenet

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

func TestCheckStreamReadyReturnsPersistentArticleFailure(t *testing.T) {
	u := &Usenet{failedFiles: xsync.NewMap[string, error]()}
	cause := &nntp.Error{
		Type:    nntp.ErrorTypeArticleNotFound,
		Code:    430,
		Message: "article missing on all providers",
	}
	u.failedFiles.Store(fsKey("nzb-id", "Movie.mkv"), cause)

	err := u.CheckStreamReady("nzb-id", "Movie.mkv")
	if err == nil {
		t.Fatal("expected previously failed file to be rejected")
	}
	if !errors.Is(err, cause) {
		t.Fatal("stream-ready error does not retain the NNTP failure")
	}
	var streamErr *customerror.Error
	if !errors.As(err, &streamErr) {
		t.Fatalf("error type = %T, want *customerror.Error", err)
	}
	if !streamErr.IsPermanent() || streamErr.HTTPStatus() != http.StatusGone {
		t.Fatalf("stream error permanent/status = %v/%d, want true/%d",
			streamErr.IsPermanent(), streamErr.HTTPStatus(), http.StatusGone)
	}

	if err := u.CheckStreamReady("nzb-id", "Other.mkv"); err != nil {
		t.Fatalf("unfailed file was rejected: %v", err)
	}
}

func TestGetOrCreateEntryRejectsFailedFileBeforeCachedReader(t *testing.T) {
	failedFiles := xsync.NewMap[string, error]()
	entries := xsync.NewMap[string, *fsEntry]()
	u := &Usenet{failedFiles: failedFiles, fs: entries}
	key := fsKey("nzb-id", "Movie.mkv")
	entries.Store(key, &fsEntry{})
	failedFiles.Store(key, &nntp.Error{
		Type:    nntp.ErrorTypeArticleNotFound,
		Code:    430,
		Message: "gone",
	})

	entry, gotKey, err := u.getOrCreateEntry(context.Background(), "nzb-id", "Movie.mkv")
	if err == nil {
		t.Fatal("expected cached reader to be fenced after a permanent stream failure")
	}
	if entry != nil {
		t.Fatal("failed file returned a cached reader")
	}
	if gotKey != key {
		t.Errorf("cache key = %q, want %q", gotKey, key)
	}
}

func TestCheckStreamReadyToleratesUninitializedFailureCache(t *testing.T) {
	if err := (*Usenet)(nil).CheckStreamReady("nzb-id", "Movie.mkv"); err != nil {
		t.Fatalf("nil usenet returned error: %v", err)
	}
	if err := (&Usenet{}).CheckStreamReady("nzb-id", "Movie.mkv"); err != nil {
		t.Fatalf("empty usenet returned error: %v", err)
	}
}
