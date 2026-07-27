package rar

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPFileReadAtRequiresExactContentRange(t *testing.T) {
	const contents = "0123456789"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=3-6" {
			t.Errorf("Range = %q, want bytes=3-6", got)
		}
		w.Header().Set("Content-Range", "bytes 3-6/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, contents[3:7])
	}))
	defer server.Close()

	file := &HttpFile{
		URL:        server.URL,
		client:     &http.Client{Timeout: time.Second},
		FileSize:   int64(len(contents)),
		MaxRetries: 0,
	}
	buffer := make([]byte, 4)
	n, err := file.ReadAt(buffer, 3)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(buffer) || string(buffer) != "3456" {
		t.Fatalf("ReadAt = %d, %q", n, buffer)
	}
}

func TestHTTPFileReadAtRejectsWrongContentRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "0123")
	}))
	defer server.Close()

	file := &HttpFile{
		URL:        server.URL,
		client:     &http.Client{Timeout: time.Second},
		FileSize:   10,
		MaxRetries: 0,
	}
	buffer := make([]byte, 4)
	if _, err := file.ReadAt(buffer, 3); !errors.Is(err, ErrNetworkError) {
		t.Fatalf("error = %v, want network integrity error", err)
	}
}

func TestHTTPFileReadAtRejectsIgnoredRangeWithoutReadingWholeBody(t *testing.T) {
	var written atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		for range 1024 {
			n, err := io.WriteString(w, strings.Repeat("x", 1024))
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	file := &HttpFile{
		URL:        server.URL,
		client:     &http.Client{Timeout: time.Second},
		FileSize:   1 << 20,
		MaxRetries: 0,
	}
	if _, err := file.ReadAt(make([]byte, 4096), 0); !errors.Is(err, ErrRangeRequestsNotSupported) {
		t.Fatalf("error = %v, want unsupported ranges", err)
	}
	if written.Load() == 0 {
		t.Fatal("test server did not start its response")
	}
}

func TestHTTPFileReadAtAcceptsExactWholeFileResponse(t *testing.T) {
	const contents = "small"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, contents)
	}))
	defer server.Close()

	file := &HttpFile{
		URL:        server.URL,
		client:     &http.Client{Timeout: time.Second},
		FileSize:   int64(len(contents)),
		MaxRetries: 0,
	}
	buffer := make([]byte, len(contents))
	n, err := file.ReadAt(buffer, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(buffer) || string(buffer) != contents {
		t.Fatalf("ReadAt = %d, %q", n, buffer)
	}
}

type secretErrorRoundTripper struct{}

func (secretErrorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, errors.New("transport exposed " + req.URL.String())
}

func TestHTTPFileNetworkErrorsRedactSignedURL(t *testing.T) {
	const secret = "signed-secret-token"
	file := &HttpFile{
		URL: "https://downloads.example.test/media.rar?token=" + secret,
		client: &http.Client{
			Transport: secretErrorRoundTripper{},
			Timeout:   time.Second,
		},
		FileSize:   10,
		MaxRetries: 0,
	}

	if _, err := file.getFileSize(); err == nil {
		t.Fatal("getFileSize() unexpectedly succeeded")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("getFileSize() leaked signed URL secret: %v", err)
	}

	if _, err := file.ReadAt(make([]byte, 4), 0); err == nil {
		t.Fatal("ReadAt() unexpectedly succeeded")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("ReadAt() leaked signed URL secret: %v", err)
	}
}
