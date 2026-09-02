package redact

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
)

// The peer is a documentation address and a .test hostname: nothing here may look like a
// real cluster (AGENTS.md §4.4), and the test asserts they are gone anyway.
const (
	addr = "192.0.2.10:6443"
	host = "api.example.test"
)

func TestErrorDropsAddressesFromNetworkErrors(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"dial refused": {
			err: &url.Error{Op: "Get", URL: "https://" + addr + "/api/v1/pods", Err: &net.OpError{
				Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 6443},
				Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
			}},
			want: "connect: connection refused",
		},
		"dns": {
			err:  &url.Error{Op: "Get", URL: "https://" + host + "/v2/", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: host}}},
			want: "dns lookup failed: no such host",
		},
		"dns timeout": {
			err:  &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true},
			want: "dns lookup timed out",
		},
		"tls hostname": {
			err:  &url.Error{Op: "Get", URL: "https://" + host, Err: &tls.CertificateVerificationError{Err: x509.HostnameError{Host: host, Certificate: &x509.Certificate{}}}},
			want: "tls: certificate is not valid for the requested host",
		},
		"tls authority": {
			err:  &url.Error{Op: "Get", URL: "https://" + host, Err: x509.UnknownAuthorityError{}},
			want: "tls: certificate signed by unknown authority",
		},
		"deadline": {
			err:  fmt.Errorf("listing: %w", &url.Error{Op: "Get", URL: "https://" + addr, Err: context.DeadlineExceeded}),
			want: "timed out",
		},
		"plain": {
			err:  errors.New("something else entirely"),
			want: "something else entirely",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Positive control: the raw error does carry the peer, so the assertion below
			// cannot pass because nothing was there to remove.
			raw := tc.err.Error()
			if name != "plain" && name != "dns timeout" && !strings.Contains(raw, addr) && !strings.Contains(raw, host) {
				t.Fatalf("control: raw error %q names neither %s nor %s", raw, addr, host)
			}
			got := Error(tc.err)
			if got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
			for _, leak := range []string{addr, host, "192.0.2.10"} {
				if strings.Contains(got, leak) {
					t.Errorf("Error() = %q still carries %s", got, leak)
				}
			}
		})
	}
}

func TestErrorHidesListedStrings(t *testing.T) {
	err := errors.New("registry said: token ghp_FAKE0000 rejected for user someone")
	got := Error(err, "ghp_FAKE0000", "someone", "")
	if strings.Contains(got, "ghp_FAKE0000") || strings.Contains(got, "someone") {
		t.Fatalf("secret survived: %q", got)
	}
	if want := "registry said: token <redacted> rejected for user <redacted>"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if Error(nil, "x") != "" {
		t.Error("nil error should print as empty")
	}
}
