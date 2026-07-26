package qbit

import (
	"net/http"
	"testing"
)

func TestSIDCookieDefaultsAreBrowserSafe(t *testing.T) {
	cookie := newSIDCookie("test-session")
	if !cookie.HttpOnly {
		t.Fatal("SID cookie must be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Fatalf("Path = %q, want /", cookie.Path)
	}
}
