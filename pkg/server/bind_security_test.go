package server

import "testing"

func TestInsecureRemoteAccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		bindAddress string
		useAuth     bool
		want        bool
	}{
		{name: "IPv4 loopback without auth", bindAddress: "127.0.0.1", want: false},
		{name: "IPv6 loopback without auth", bindAddress: "::1", want: false},
		{name: "bracketed IPv6 loopback without auth", bindAddress: "[::1]", want: false},
		{name: "zoned IPv6 loopback without auth", bindAddress: "::1%lo", want: false},
		{name: "localhost without auth", bindAddress: "LOCALHOST", want: false},
		{name: "all IPv4 interfaces without auth", bindAddress: "0.0.0.0", want: true},
		{name: "all IPv6 interfaces without auth", bindAddress: "::", want: true},
		{name: "private interface without auth", bindAddress: "10.0.0.4", want: true},
		{name: "public interface with auth", bindAddress: "0.0.0.0", useAuth: true, want: false},
		{name: "empty address without auth", bindAddress: "", want: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := insecureRemoteAccess(test.bindAddress, test.useAuth); got != test.want {
				t.Fatalf("insecureRemoteAccess(%q, %t) = %t, want %t", test.bindAddress, test.useAuth, got, test.want)
			}
		})
	}
}
