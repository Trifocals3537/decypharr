package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckDataReadinessRequiresOK(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{name: "ready", status: http.StatusOK, want: true},
		{name: "starting", status: http.StatusServiceUnavailable, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/ready" {
					t.Fatalf("path = %q, want /ready", r.URL.Path)
				}
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatalf("split test server address: %v", err)
			}
			got := checkDataReadiness(context.Background(), server.Client(), "/", port)
			if got != tt.want {
				t.Fatalf("checkDataReadiness() = %v, want %v", got, tt.want)
			}
		})
	}
}
