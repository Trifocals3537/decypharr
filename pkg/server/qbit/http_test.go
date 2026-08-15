package qbit

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSIDCookieDefaultsAreBrowserSafe(t *testing.T) {
	cookie := newSIDCookie("test-session", true)
	if !cookie.HttpOnly {
		t.Fatal("SID cookie must be HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Fatalf("Path = %q, want /", cookie.Path)
	}
	if !cookie.Secure {
		t.Fatal("SID cookie must be Secure for an HTTPS listener")
	}
	if cookie.MaxAge != int(qbitSessionTTL/time.Second) {
		t.Fatalf("MaxAge = %d, want %d", cookie.MaxAge, int(qbitSessionTTL/time.Second))
	}
}

func TestQbitCookieSecurityFollowsActualTransport(t *testing.T) {
	httpRequest := httptest.NewRequest(http.MethodPost, "http://private.example/api/v2/auth/login", nil)
	if qbitCookieSecure(httpRequest) {
		t.Fatal("private HTTP compatibility client received a Secure-only cookie")
	}

	httpsRequest := httptest.NewRequest(http.MethodPost, "https://private.example/api/v2/auth/login", nil)
	httpsRequest.TLS = &tls.ConnectionState{}
	if !qbitCookieSecure(httpsRequest) {
		t.Fatal("TLS compatibility client did not receive a Secure cookie")
	}
}

func TestNormalizeStateFilterTreatsAllAsUnfiltered(t *testing.T) {
	for _, raw := range []string{"all", " ALL ", ""} {
		if got := normalizeStateFilter(raw); got != "" {
			t.Fatalf("normalizeStateFilter(%q) = %q, want empty", raw, got)
		}
	}
	if got := normalizeStateFilter(" downloading "); got != "downloading" {
		t.Fatalf("normalizeStateFilter() = %q, want downloading", got)
	}
}
