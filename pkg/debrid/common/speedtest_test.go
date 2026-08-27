package common

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/request"
)

func TestMain(m *testing.M) {
	configDir, err := os.MkdirTemp("", "decypharr-common-test-")
	if err != nil {
		panic(err)
	}
	config.SetConfigPath(configDir)
	code := m.Run()
	_ = os.RemoveAll(configDir)
	os.Exit(code)
}

func TestProbeDownloadUsesCredentialFreeBoundedRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want no provider API credential", got)
		}
		if got := r.Header.Get("User-Agent"); got != "Decypharr-Test" {
			t.Errorf("User-Agent = %q, want non-secret client header preserved", got)
		}
		if got := r.Header.Get("Range"); got != "bytes=0-1048575" {
			t.Errorf("Range = %q, want one-megabyte probe", got)
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
	}))
	defer server.Close()

	client := request.New(
		request.WithHeaders(map[string]string{
			"Authorization": "Bearer secret",
			"User-Agent":    "Decypharr-Test",
		}),
		request.WithMaxRetries(0),
	)

	bytesRead, duration, err := ProbeDownload(t.Context(), client, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if bytesRead != 4096 {
		t.Fatalf("bytesRead = %d, want 4096", bytesRead)
	}
	if duration <= 0 {
		t.Fatalf("duration = %s, want positive duration", duration)
	}
}

func TestProbeDownloadRejectsErrorAndEmptyResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "error status", status: http.StatusUnauthorized, body: "credential rejected"},
		{name: "empty success", status: http.StatusPartialContent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			client := request.New(request.WithMaxRetries(0))
			bytesRead, _, err := ProbeDownload(t.Context(), client, server.URL)
			if err == nil {
				t.Fatalf("ProbeDownload() bytes = %d, want error", bytesRead)
			}
			if bytesRead != 0 {
				t.Fatalf("bytesRead = %d, want 0 on invalid response", bytesRead)
			}
		})
	}
}
