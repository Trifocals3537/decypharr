package premiumize

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/request"
)

func TestDoRecognizesRedactedAPIErrorEnvelopeWithoutOutput(t *testing.T) {
	const secret = "https://cdn.invalid/file?token=do-not-expose"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","code":"permission_denied","message":"` + secret + `"}`))
	}))
	defer server.Close()

	client := &Premiumize{Host: server.URL, client: request.New(request.WithMaxRetries(0))}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.do(req, nil)
	var apiErr *premiumizeAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != "permission_denied" {
		t.Fatalf("do error = %v, want typed permission_denied error", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "do-not-expose") {
		t.Fatalf("do error exposed provider message: %v", err)
	}
}

func TestAPIErrorDoesNotReflectUnexpectedCode(t *testing.T) {
	err := (&premiumizeAPIError{Code: "token=do-not-expose"}).Error()
	if strings.Contains(err, "do-not-expose") || !strings.Contains(err, "unknown_error") {
		t.Fatalf("error = %q, want a redacted unknown code", err)
	}
}

func TestDeleteTorrentMapsAPIEnvelopeNotFound(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","code":"not_found","message":"Transfer not found"}`))
	}))
	defer server.Close()

	client := &Premiumize{
		Host:   server.URL,
		client: request.New(request.WithMaxRetries(0)),
		logger: zerolog.Nop(),
	}
	if err := client.DeleteTorrent("missing"); !errors.Is(err, customerror.TorrentNotFoundError) {
		t.Fatalf("DeleteTorrent error = %v, want TorrentNotFoundError", err)
	}
}

func TestCheckFileItemDetailsHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	client := &Premiumize{
		Host:   server.URL,
		client: request.New(request.WithMaxRetries(0), request.WithTimeout(time.Second)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- client.CheckFile(ctx, "", "item-id") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("item details request did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("CheckFile error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CheckFile did not return after context cancellation")
	}
}

func TestGetProfileCacheIsConcurrentAndReturnsCopies(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","customer_id":"42","premium_until":4102444800,"limit_used":0.5,"booster_points":7}`))
	}))
	defer server.Close()

	client := &Premiumize{
		Host:   server.URL,
		client: request.New(request.WithMaxRetries(0)),
		config: config.Debrid{Name: "premiumize-main"},
	}
	const callers = 32
	profiles := make(chan string, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			profile, err := client.GetProfile()
			if err != nil {
				errs <- err
				return
			}
			profiles <- profile.Username
		}()
	}
	wg.Wait()
	close(errs)
	close(profiles)
	for err := range errs {
		t.Fatalf("GetProfile error = %v", err)
	}
	for username := range profiles {
		if username != "42" {
			t.Fatalf("profile username = %q, want 42", username)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("account requests = %d, want 1", got)
	}

	first, err := client.GetProfile()
	if err != nil {
		t.Fatal(err)
	}
	first.Username = "mutated"
	second, err := client.GetProfile()
	if err != nil {
		t.Fatal(err)
	}
	if second.Username != "42" {
		t.Fatalf("cached profile was mutated through caller copy: %#v", second)
	}
}

func TestGetProfileRejectsMalformedCustomerID(t *testing.T) {
	config.SetConfigPath(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","customer_id":"not-a-number"}`))
	}))
	defer server.Close()

	client := &Premiumize{Host: server.URL, client: request.New(request.WithMaxRetries(0))}
	if _, err := client.GetProfile(); err == nil || !strings.Contains(err.Error(), "invalid customer_id") {
		t.Fatalf("GetProfile error = %v, want invalid customer_id", err)
	}
}

func TestFlexibleUnixTimeRejectsInvalidString(t *testing.T) {
	var timestamp flexibleUnixTime
	if err := json.Unmarshal([]byte(`"not-a-timestamp"`), &timestamp); err == nil {
		t.Fatal("Unmarshal error = nil, want invalid timestamp error")
	}
}

func TestNormalizeProgressBoundsProviderValues(t *testing.T) {
	for _, test := range []struct {
		name string
		in   float64
		want float64
	}{
		{name: "negative", in: -0.1, want: 0},
		{name: "not a number", in: math.NaN(), want: 0},
		{name: "fraction", in: 0.42, want: 42},
		{name: "one", in: 1, want: 100},
		{name: "percent", in: 50, want: 50},
		{name: "too large", in: 150, want: 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeProgress(test.in); got != test.want {
				t.Fatalf("normalizeProgress(%v) = %v, want %v", test.in, got, test.want)
			}
		})
	}
}
