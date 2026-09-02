// Package redact turns transport errors into messages that carry no address and no secret.
//
// pkg/k8s and pkg/registry both wrap network clients whose errors embed the peer they
// talked to: a *url.Error carries the full URL, a *net.OpError the dial address, a
// *net.DNSError the name looked up, an x509 error the hostnames on the certificate. Those
// strings are exactly what AGENTS.md §4.4 keeps out of every public surface (R-004), so no
// adaptor returns a client error unchanged. Error walks the wrap chain down to the cause
// that says *what* went wrong (refused, timed out, no such host, unknown authority) and
// drops every field that says *where*. The adaptors add the where themselves, in terms
// they are allowed to print: a context name, a namespace, a registry host from an image
// reference the operator already holds.
//
// The hide list is the second guard (R-002): any of those strings, typically credentials
// the caller has in hand, is replaced wherever it appears in the final message. Both guards
// are structural — a new error type that slips past the type switch still cannot carry a
// listed secret, and the tests pin both.
//
// Register adds a third guard, process-wide: pkg/k8s and pkg/registry call it the moment
// they load a credential value (an env token, a cluster secret's password, `op`'s output),
// independent of whichever local hide list the call site that later errors remembers to
// thread through. Every Error and Strings call scrubs the registered set in addition to
// its own hide arguments, so a message built two or three calls away from where the
// credential was read — a warning in pkg/resolve, the CLI's plan printer — is still caught
// at the process boundary rather than only at the adaptor that happened to load it.
// Register("") is a deliberate no-op: an empty string must never become "hide everything".
package redact

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
)

// Redacted replaces every hidden string in a message.
const Redacted = "<redacted>"

var (
	mu     sync.RWMutex
	hidden = map[string]struct{}{}
)

// Register adds value to the process-wide set every subsequent Error and Strings call
// scrubs. value == "" is a no-op — it must never make Strings redact everything.
func Register(value string) {
	if value == "" {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	hidden[value] = struct{}{}
}

// registered snapshots the process-wide set.
func registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(hidden))
	for h := range hidden {
		out = append(out, h)
	}
	return out
}

// Error returns a message for err with addresses, hostnames and URLs removed and every
// non-empty string in hide, plus every value ever passed to Register, replaced by
// Redacted. A nil err yields "".
func Error(err error, hide ...string) string {
	if err == nil {
		return ""
	}
	return Strings(describe(err), hide...)
}

// Strings replaces every non-empty string in hide, and every value ever passed to
// Register, within s.
func Strings(s string, hide ...string) string {
	for _, h := range hide {
		if h != "" {
			s = strings.ReplaceAll(s, h, Redacted)
		}
	}
	for _, h := range registered() {
		s = strings.ReplaceAll(s, h, Redacted)
	}
	return s
}

// Host returns every spelling of a server address worth scrubbing from a message: the
// string as given, its host[:port] with any "scheme://" prefix stripped, and the bare
// host alone with the port removed too. The last form matters because a typed error
// (x509.HostnameError and the like) is already rendered as static text by describe, but an
// error that reaches this package by a route the type switch doesn't cover can still spell
// the bare host as plain text — "x509: certificate is valid for …, not 192.0.2.10" — and a
// hide list built only from host:port never matches that. raw == "" returns nil.
func Host(raw string) []string {
	if raw == "" {
		return nil
	}
	out := []string{raw}
	hostport := raw
	if i := strings.Index(hostport, "://"); i >= 0 {
		hostport = hostport[i+3:]
		out = append(out, hostport)
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host != "" && host != hostport {
		out = append(out, host)
	}
	return out
}

// describe peels the error down to a cause with no address in it.
func describe(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	var (
		urlErr  *url.Error
		opErr   *net.OpError
		dnsErr  *net.DNSError
		sysErr  *os.SyscallError
		hostErr x509.HostnameError
		caErr   x509.UnknownAuthorityError
		certErr x509.CertificateInvalidError
		tlsErr  *tls.CertificateVerificationError
	)
	switch {
	case errors.As(err, &dnsErr):
		// DNSError.Error() prints the name looked up; Err alone is "no such host" etc.
		if dnsErr.IsTimeout {
			return "dns lookup timed out"
		}
		return "dns lookup failed: " + dnsErr.Err
	case errors.As(err, &hostErr):
		return "tls: certificate is not valid for the requested host"
	case errors.As(err, &caErr):
		return "tls: certificate signed by unknown authority"
	case errors.As(err, &certErr):
		return "tls: invalid certificate"
	case errors.As(err, &tlsErr):
		return "tls: " + describe(tlsErr.Err)
	case errors.As(err, &sysErr):
		return sysErr.Syscall + ": " + sysErr.Err.Error()
	case errors.As(err, &opErr):
		// OpError.Error() prints Addr; Op plus the inner cause is enough.
		if opErr.Err == nil {
			return opErr.Op + " failed"
		}
		return opErr.Op + ": " + describe(opErr.Err)
	case errors.As(err, &urlErr):
		// url.Error.Error() prints the URL; keep the operation and the cause.
		if urlErr.Err == nil {
			return urlErr.Op + " failed"
		}
		return strings.ToLower(urlErr.Op) + ": " + describe(urlErr.Err)
	}
	return err.Error()
}
