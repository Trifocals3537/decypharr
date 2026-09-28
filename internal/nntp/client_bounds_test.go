package nntp

import (
	"testing"

	"github.com/Trifocals3537/tessarr/internal/config"
)

func TestNewClientDefensivelyRejectsUnsafeProviderCapacity(t *testing.T) {
	for _, capacity := range []int{-1, config.UsenetConnectionLimit + 1} {
		cfg := &config.Config{Usenet: config.Usenet{Providers: []config.UsenetProvider{{
			Host:           "news.example.test",
			MaxConnections: capacity,
		}}}}
		if client, err := NewClient(cfg); err == nil || client != nil {
			t.Fatalf("NewClient(max_connections=%d) = (%v, %v), want rejection", capacity, client, err)
		}
	}
}
