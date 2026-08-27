package qbit

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

func TestContainsAllHashes(t *testing.T) {
	if !containsAllHashes([]string{"hash", " ALL "}) {
		t.Fatal("containsAllHashes() did not recognize all")
	}
	if containsAllHashes([]string{"hash-a", "hash-b"}) {
		t.Fatal("containsAllHashes() matched ordinary hashes")
	}
}

func TestShouldDeleteTorrentFilesRequiresExplicitTrue(t *testing.T) {
	for _, test := range []struct {
		name string
		form url.Values
		want bool
	}{
		{name: "missing", form: url.Values{}},
		{name: "false", form: url.Values{"deleteFiles": {"false"}}},
		{name: "explicit true", form: url.Values{"deleteFiles": {"true"}}, want: true},
		{name: "case and whitespace", form: url.Values{"deleteFiles": {" TRUE "}}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v2/torrents/delete",
				strings.NewReader(test.form.Encode()),
			)
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if got := shouldDeleteTorrentFiles(request); got != test.want {
				t.Fatalf("shouldDeleteTorrentFiles() = %t, want %t", got, test.want)
			}
		})
	}
}
