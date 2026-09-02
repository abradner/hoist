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

// The untyped-error case this package doc calls out: a hostname mismatch that reaches
// describe by a route the type switch doesn't cover (a plain string, type information
// already lost) still spells the bare host as plain text. A hide list built only from
// host:port never matches that; Host must offer the bare host alone too.
func TestHostScrubsFreeTextBareHost(t *testing.T) {
	raw := "https://192.0.2.10:6443"
	hide := Host(raw)
	untyped := errors.New("x509: certificate is valid for kube.example.test, not 192.0.2.10")
	got := Strings(untyped.Error(), hide...)
	for _, leak := range []string{"192.0.2.10"} {
		if strings.Contains(got, leak) {
			t.Errorf("Host(%q) = %v did not scrub the bare host from %q: got %q", raw, hide, untyped.Error(), got)
		}
	}
	// Every spelling the caller could have configured is covered: with the scheme, the
	// host:port alone, and the bare host with no port.
	for _, want := range []string{raw, "192.0.2.10:6443", "192.0.2.10"} {
		found := false
		for _, h := range hide {
			found = found || h == want
		}
		if !found {
			t.Errorf("Host(%q) = %v lacks %q", raw, hide, want)
		}
	}
	if got := Host(""); got != nil {
		t.Errorf("Host(\"\") = %v, want nil", got)
	}
}

// Register is the third guard: a value registered anywhere in the process is scrubbed by
// every later Strings/Error call, even with no local hide argument — and Register("") must
// never turn into "redact everything".
func TestRegisterScrubsProcessWide(t *testing.T) {
	const secret = "REGISTER-TEST-SECRET-4f8c2a"
	Register(secret)
	t.Cleanup(func() {
		mu.Lock()
		delete(hidden, secret)
		mu.Unlock()
	})
	got := Strings("the response carried " + secret + " in the clear")
	if strings.Contains(got, secret) {
		t.Errorf("registered secret survived with no local hide list: %q", got)
	}
	Register("")
	if got := Strings("nothing registered lives here"); got != "nothing registered lives here" {
		t.Errorf("Register(\"\") redacted something: %q", got)
	}
}
