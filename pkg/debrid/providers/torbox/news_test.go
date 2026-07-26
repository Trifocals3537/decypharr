package torbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/request"
)

func TestProvisionNewsServerCreatedAccount(t *testing.T) {
	client, closeServer := newNewsServerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/usenet/provider/account" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
			return
		}
		writeNewsServerResponse(
			t,
			w,
			true,
			nil,
			NewsServerAccount{
				Host:        "nntp.torbox.app",
				Port:        563,
				SSL:         true,
				Connections: 10,
				Username:    "auth-id",
				Password:    "one-time-secret",
			},
		)
	})
	defer closeServer()

	provision, err := client.ProvisionNewsServer(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !provision.Created {
		t.Fatal("Created = false, want true")
	}
	provider := provision.Provider
	if provider.Host != "nntp.torbox.app" ||
		provider.Port != 563 ||
		!provider.SSL ||
		provider.MaxConnections != 10 ||
		provider.Username != "auth-id" ||
		provider.Password != "one-time-secret" {
		t.Fatalf("provider = %#v", provider)
	}
}

func TestProvisionNewsServerExistingAccountRequiresPassword(t *testing.T) {
	client, closeServer := newNewsServerTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeNewsServerResponse(
			t,
			w,
			true,
			"DUPLICATE_ITEM",
			NewsServerAccount{
				Host:        "nntp.torbox.app",
				Port:        563,
				SSL:         true,
				Connections: 10,
				Username:    "auth-id",
				Password:    "********",
			},
		)
	})
	defer closeServer()

	provision, err := client.ProvisionNewsServer(context.Background(), "")
	if !errors.Is(err, ErrNewsServerPasswordUnavailable) || provision != nil {
		t.Fatalf("provision = %#v, error = %v", provision, err)
	}

	provision, err = client.ProvisionNewsServer(context.Background(), "existing-secret")
	if err != nil {
		t.Fatal(err)
	}
	if provision.Created {
		t.Fatal("Created = true, want false")
	}
	if provision.Provider.Password != "existing-secret" {
		t.Fatalf("password = %q", provision.Provider.Password)
	}
}

func TestProvisionNewsServerValidatesAndBoundsProvider(t *testing.T) {
	tests := []struct {
		name        string
		account     NewsServerAccount
		wantError   string
		wantMaxConn int
	}{
		{
			name: "connection ceiling",
			account: NewsServerAccount{
				Host:        "eu.nntp.torbox.app.",
				Port:        563,
				SSL:         true,
				Connections: 1000,
				Username:    "auth-id",
				Password:    "secret",
			},
			wantMaxConn: 10,
		},
		{
			name: "foreign host",
			account: NewsServerAccount{
				Host:        "torbox.app.attacker.invalid",
				Port:        563,
				SSL:         true,
				Connections: 10,
				Username:    "auth-id",
				Password:    "secret",
			},
			wantError: "unexpected server host",
		},
		{
			name: "plaintext",
			account: NewsServerAccount{
				Host:        "nntp.torbox.app",
				Port:        119,
				SSL:         false,
				Connections: 10,
				Username:    "auth-id",
				Password:    "secret",
			},
			wantError: "insecure connection",
		},
		{
			name: "invalid connection count",
			account: NewsServerAccount{
				Host:        "nntp.torbox.app",
				Port:        563,
				SSL:         true,
				Connections: 0,
				Username:    "auth-id",
				Password:    "secret",
			},
			wantError: "invalid connection limit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, closeServer := newNewsServerTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeNewsServerResponse(t, w, true, nil, test.account)
			})
			defer closeServer()

			provision, err := client.ProvisionNewsServer(context.Background(), "")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if provision.Provider.MaxConnections != test.wantMaxConn {
				t.Fatalf(
					"MaxConnections = %d, want %d",
					provision.Provider.MaxConnections,
					test.wantMaxConn,
				)
			}
		})
	}
}

func TestProvisionNewsServerDoesNotExposeProviderResponse(t *testing.T) {
	client, closeServer := newNewsServerTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"do-not-log","password":"secret"}`))
	})
	defer closeServer()

	_, err := client.ProvisionNewsServer(context.Background(), "")
	if err == nil ||
		strings.Contains(err.Error(), "do-not-log") ||
		strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v", err)
	}
}

func TestProvisionNewsServerBoundsCredentialResponse(t *testing.T) {
	client, closeServer := newNewsServerTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"host":"nntp.torbox.app","password":"`))
		_, _ = w.Write([]byte(strings.Repeat("x", maxNewsServerResponseSize)))
		_, _ = w.Write([]byte(`"}}`))
	})
	defer closeServer()

	_, err := client.ProvisionNewsServer(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("error = %v", err)
	}
}

func TestResetNewsServerPasswordIsExplicitPOST(t *testing.T) {
	client, closeServer := newNewsServerTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/usenet/provider/account/resetpw" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
			return
		}
		writeNewsServerResponse(
			t,
			w,
			true,
			nil,
			NewsServerAccount{
				Host:        "nntp.torbox.app",
				Port:        563,
				SSL:         true,
				Connections: 10,
				Username:    "auth-id",
				Password:    "replacement-secret",
			},
		)
	})
	defer closeServer()

	provider, err := client.ResetNewsServerPassword(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if provider.Password != "replacement-secret" {
		t.Fatalf("password = %q", provider.Password)
	}
}

func TestNewsServerRequestsRequireContext(t *testing.T) {
	client := &Torbox{}
	if _, err := client.ProvisionNewsServer(nil, "password"); err == nil {
		t.Fatal("ProvisionNewsServer(nil) succeeded")
	}
	if _, err := client.ResetNewsServerPassword(nil); err == nil {
		t.Fatal("ResetNewsServerPassword(nil) succeeded")
	}
}

func newNewsServerTestClient(
	t *testing.T,
	handler http.HandlerFunc,
) (*Torbox, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	client := &Torbox{
		Host:   server.URL,
		APIKey: "test-key",
		client: request.New(
			request.WithHeaders(map[string]string{
				"Authorization": "Bearer test-key",
			}),
			request.WithMaxRetries(0),
		),
	}
	return client, server.Close
}

func writeNewsServerResponse(
	t *testing.T,
	w http.ResponseWriter,
	success bool,
	apiError any,
	account NewsServerAccount,
) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"success": success,
		"error":   apiError,
		"data":    account,
	}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
