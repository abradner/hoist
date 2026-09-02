// Package registry resolves tags to digests and lists tags, behind an ordered, explicit
// credential chain.
//
// It is the "Container registry" surface of docs/repo-map.md: read-only, and the only
// place registry credentials exist (R-002). A credential is obtained from one source at a
// time — env, keychain, cluster, op, in the caller's order, then anonymous — and tried
// against the registry; the first source whose request succeeds is remembered for the
// life of the Client and reported by name through AuthSourceUsed ("cluster secret
// app-staging/ghcr-pull"). No input, output, error or report carries a token, a password
// or a username read from a source: registry errors are reduced to their status and error
// codes (never the response body), transport errors go through pkg/redact, and every
// credential value the chain has seen is scrubbed from every message as a second guard.
//
// The op source runs a program (`op read <ref>`). It runs only when the caller configured
// an OpRef and listed op in the order; an unconfigured source is skipped and reported as
// "not configured", never guessed at (AGENTS.md §4.3).
//
// Head returns the digest a pull by tag would pin: for a multi-arch image that is the
// image index digest, which is also what the container runtime reports in a pod's
// imageID for the same tag (containerd and Docker both record the digest of the manifest
// the tag resolved to). TestHeadMultiArchReturnsIndexDigest pins the choice.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
	"github.com/abradner/hoist/pkg/redact"
)

// Registry is what the resolver needs from a registry.
type Registry interface {
	// Head returns the digest of ref (repo:tag, or repo@digest which is returned as is
	// after the registry confirms it). See the package doc for multi-arch images.
	Head(ctx context.Context, ref image.Ref) (digest string, err error)
	// Tags lists the tags of repo, sorted; registries return them in no useful order
	// (AGENTS.md §6.1).
	Tags(ctx context.Context, repo string) ([]string, error)
}

// AuthReporter is implemented by a Registry that can say which credential source
// authenticated, by name only.
type AuthReporter interface {
	AuthSourceUsed() string
}

// AuthSource names one link of the credential chain.
type AuthSource string

// The credential sources, in their default order.
const (
	AuthEnv      AuthSource = "env"      // HOIST_GHCR_TOKEN, then GHCR_TOKEN; ghcr.io only
	AuthKeychain AuthSource = "keychain" // ~/.docker/config.json and credential helpers
	AuthCluster  AuthSource = "cluster"  // a kubernetes.io/dockerconfigjson Secret, opt-in
	AuthOp       AuthSource = "op"       // `op read <ref>`, opt-in
)

// DefaultAuthOrder is the chain when the caller states none.
var DefaultAuthOrder = []AuthSource{AuthEnv, AuthKeychain, AuthCluster, AuthOp}

// ParseAuthOrder turns a list of source names into an order, refusing unknown names and
// duplicates.
func ParseAuthOrder(names []string) ([]AuthSource, error) {
	var out []AuthSource
	seen := map[AuthSource]bool{}
	for _, n := range names {
		s := AuthSource(strings.TrimSpace(n))
		switch s {
		case AuthEnv, AuthKeychain, AuthCluster, AuthOp:
		default:
			return nil, fmt.Errorf("unknown registry auth source %q (want env, keychain, cluster or op)", n)
		}
		if seen[s] {
			return nil, fmt.Errorf("registry auth source %q listed twice", n)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

// AuthConfig is how a Client obtains credentials. It names sources; it holds no secret.
type AuthConfig struct {
	// Order is the chain; empty means DefaultAuthOrder.
	Order []AuthSource
	// ClusterSecret is "namespace/name" of the pull secret the cluster source reads.
	// Empty means the cluster source is skipped.
	ClusterSecret string
	// OpRef is the 1Password reference the op source reads. Empty means op is skipped and
	// nothing is executed.
	OpRef string
	// Cluster reads ClusterSecret; required when ClusterSecret is set.
	Cluster k8s.Cluster
	// Keychain is the keychain source; nil means go-containerregistry's DefaultKeychain
	// (Docker config and credential helpers). Tests substitute one.
	Keychain authn.Keychain
	// Transport is the HTTP transport; nil means go-containerregistry's default. Tests
	// point it at an in-memory registry.
	Transport http.RoundTripper
}

// Client is Registry over go-containerregistry with the credential chain of AuthConfig.
type Client struct {
	cfg AuthConfig

	mu      sync.Mutex
	winners map[string]link // registry host → the link that worked there
	cluster *cached[authn.Keychain]
	op      *cached[string]
}

// cached is a one-shot result: the cluster secret is read once, op is run once.
type cached[T any] struct {
	done bool
	val  T
	err  error
}

// link is one credential candidate for one host.
type link struct {
	source AuthSource
	name   string // how the outcome is reported: "env (GHCR_TOKEN)", "cluster secret ns/name"
	auth   authn.Authenticator
	hide   []string // the values that must never reach a message
}

// fixedUser is the username sent with a bare token when no user is configured. GHCR
// ignores the username for a token; it is not a credential and is not redacted.
const fixedUser = "hoist"

// New validates cfg and returns a Client. Nothing is contacted or executed here.
func New(cfg AuthConfig) (*Client, error) {
	if len(cfg.Order) == 0 {
		cfg.Order = append([]AuthSource(nil), DefaultAuthOrder...)
	}
	seen := map[AuthSource]bool{}
	for _, s := range cfg.Order {
		switch s {
		case AuthEnv, AuthKeychain, AuthCluster, AuthOp:
		default:
			return nil, fmt.Errorf("registry: unknown auth source %q", s)
		}
		if seen[s] {
			return nil, fmt.Errorf("registry: auth source %q listed twice", s)
		}
		seen[s] = true
	}
	if cfg.ClusterSecret != "" {
		ns, n, ok := strings.Cut(cfg.ClusterSecret, "/")
		if !ok || ns == "" || n == "" || strings.Contains(n, "/") {
			return nil, fmt.Errorf("registry: cluster secret %q: want namespace/name", cfg.ClusterSecret)
		}
		if cfg.Cluster == nil {
			return nil, errors.New("registry: cluster secret named but no cluster to read it from")
		}
	}
	if cfg.Keychain == nil {
		cfg.Keychain = authn.DefaultKeychain
	}
	if cfg.Transport == nil {
		cfg.Transport = remote.DefaultTransport
	}
	return &Client{cfg: cfg, winners: map[string]link{}, cluster: &cached[authn.Keychain]{}, op: &cached[string]{}}, nil
}

// AuthSourceUsed implements AuthReporter: the winning link per registry host, by name,
// or "" when no request has succeeded yet. With one host it is just the link
// ("cluster secret app-staging/ghcr-pull"); with several, "host: link; host: link".
func (c *Client) AuthSourceUsed() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.winners) == 1 {
		for _, l := range c.winners {
			return l.name
		}
	}
	hosts := make([]string, 0, len(c.winners))
	for h := range c.winners {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		parts = append(parts, h+": "+c.winners[h].name)
	}
	return strings.Join(parts, "; ")
}

// Head implements Registry.
func (c *Client) Head(ctx context.Context, ref image.Ref) (string, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	nref, err := name.ParseReference(ref.String())
	if err != nil {
		return "", fmt.Errorf("registry: %s: %w", ref, err)
	}
	var digest string
	err = c.do(ctx, nref.Context().Registry, func(a authn.Authenticator) error {
		d, err := remote.Head(nref, remote.WithContext(ctx), remote.WithAuth(a), remote.WithTransport(c.cfg.Transport))
		if err != nil {
			return err
		}
		digest = d.Digest.String()
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("registry: %s: %w", ref, err)
	}
	if err := (image.Ref{Repo: ref.Repo, Digest: digest}).Validate(); err != nil {
		return "", fmt.Errorf("registry: %s: returned %w", ref, err)
	}
	return digest, nil
}

// Tags implements Registry.
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error) {
	nrepo, err := name.NewRepository(repo)
	if err != nil {
		return nil, fmt.Errorf("registry: %s: %w", repo, err)
	}
	var tags []string
	err = c.do(ctx, nrepo.Registry, func(a authn.Authenticator) error {
		t, err := remote.List(nrepo, remote.WithContext(ctx), remote.WithAuth(a), remote.WithTransport(c.cfg.Transport))
		if err != nil {
			return err
		}
		tags = t
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("registry: %s: %w", repo, err)
	}
	sort.Strings(tags)
	return tags, nil
}

// do runs op with the remembered credential for reg, or walks the chain until one link's
// request succeeds. A 401/403 moves to the next link; any other failure is reported at
// once, since the credential was not what was wrong.
func (c *Client) do(ctx context.Context, reg name.Registry, op func(authn.Authenticator) error) error {
	host := reg.RegistryStr()
	c.mu.Lock()
	defer c.mu.Unlock()
	if w, ok := c.winners[host]; ok {
		if err := op(w.auth); err != nil {
			return errors.New(describe(err, w.hide))
		}
		return nil
	}
	var outcomes []string
	var hide []string
	for _, source := range append(append([]AuthSource(nil), c.cfg.Order...), "") {
		l, skip := c.link(ctx, source, reg)
		if skip != "" {
			outcomes = append(outcomes, skip)
			continue
		}
		hide = append(hide, l.hide...)
		err := op(l.auth)
		if err == nil {
			c.winners[host] = l
			return nil
		}
		msg := describe(err, hide)
		if !authFailure(err) {
			return fmt.Errorf("%s (auth: %s)", msg, l.name)
		}
		outcomes = append(outcomes, l.name+": "+msg)
	}
	return fmt.Errorf("no credential source worked for %s: %s", host, redact.Strings(strings.Join(outcomes, "; "), hide...))
}

// link builds the credential for one source and host, or says why the source is skipped.
// The empty source is the implicit anonymous attempt that ends every chain.
func (c *Client) link(ctx context.Context, source AuthSource, reg name.Registry) (link, string) {
	host := reg.RegistryStr()
	switch source {
	case "":
		return link{name: "anonymous", auth: authn.Anonymous}, ""
	case AuthEnv:
		if host != "ghcr.io" {
			return link{}, "env: only for ghcr.io, not " + host
		}
		for _, v := range []string{"HOIST_GHCR_TOKEN", "GHCR_TOKEN"} {
			if tok := os.Getenv(v); tok != "" {
				user, hide := tokenUser(tok)
				return link{source: source, name: "env (" + v + ")", auth: &authn.Basic{Username: user, Password: tok}, hide: hide}, ""
			}
		}
		return link{}, "env: neither HOIST_GHCR_TOKEN nor GHCR_TOKEN is set"
	case AuthKeychain:
		a, err := authn.Resolve(ctx, c.cfg.Keychain, reg)
		if err != nil {
			return link{}, "keychain: " + redact.Error(err)
		}
		if a == authn.Anonymous {
			return link{}, "keychain: no entry for " + host
		}
		return link{source: source, name: "keychain", auth: a, hide: hideOf(ctx, a)}, ""
	case AuthCluster:
		if c.cfg.ClusterSecret == "" {
			return link{}, "cluster: not configured"
		}
		which := "cluster secret " + c.cfg.ClusterSecret
		if !c.cluster.done {
			ns, n, _ := strings.Cut(c.cfg.ClusterSecret, "/")
			c.cluster.val, c.cluster.err = c.cfg.Cluster.DockerConfigSecret(ctx, ns, n)
			c.cluster.done = true
		}
		if c.cluster.err != nil {
			return link{}, c.cluster.err.Error()
		}
		a, err := authn.Resolve(ctx, c.cluster.val, reg)
		if err != nil {
			return link{}, which + ": " + redact.Error(err)
		}
		if a == authn.Anonymous {
			return link{}, which + ": no entry for " + host
		}
		return link{source: source, name: which, auth: a, hide: hideOf(ctx, a)}, ""
	case AuthOp:
		if c.cfg.OpRef == "" {
			return link{}, "op: not configured"
		}
		if !c.op.done {
			c.op.val, c.op.err = opRead(ctx, c.cfg.OpRef)
			c.op.val = strings.TrimSpace(c.op.val)
			c.op.done = true
		}
		if c.op.err != nil {
			return link{}, "op: " + c.op.err.Error()
		}
		user, hide := tokenUser(c.op.val)
		return link{source: source, name: "op", auth: &authn.Basic{Username: user, Password: c.op.val}, hide: hide}, ""
	}
	return link{}, string(source) + ": unknown source"
}

// tokenUser picks the username sent with a bare token: HOIST_GHCR_USER, GHCR_USER, else
// fixedUser. It returns the strings to hide: the token, and the user when it came from
// the environment.
func tokenUser(token string) (string, []string) {
	for _, v := range []string{"HOIST_GHCR_USER", "GHCR_USER"} {
		if u := os.Getenv(v); u != "" {
			return u, []string{token, u}
		}
	}
	return fixedUser, []string{token}
}

// hideOf lists the credential values an authenticator would send.
func hideOf(ctx context.Context, a authn.Authenticator) []string {
	cfg, err := authn.Authorization(ctx, a)
	if err != nil || cfg == nil {
		return nil
	}
	var out []string
	for _, v := range []string{cfg.Username, cfg.Password, cfg.Auth, cfg.IdentityToken, cfg.RegistryToken} {
		if v != "" && v != fixedUser {
			out = append(out, v)
		}
	}
	return out
}

// authFailure reports whether the registry refused the credential (as opposed to the
// tag being missing or the network failing): HTTP 401/403, or an UNAUTHORIZED/DENIED code.
func authFailure(err error) bool {
	var te *transport.Error
	if !errors.As(err, &te) {
		return false
	}
	if te.StatusCode == http.StatusUnauthorized || te.StatusCode == http.StatusForbidden {
		return true
	}
	for _, d := range te.Errors {
		if d.Code == transport.UnauthorizedErrorCode || d.Code == transport.DeniedErrorCode {
			return true
		}
	}
	return false
}

// describe renders a request error with nothing from the response body: the status and
// the registry's error codes for a registry error, the redacted cause otherwise. Every
// string in hide is scrubbed afterwards.
func describe(err error, hide []string) string {
	var te *transport.Error
	if errors.As(err, &te) {
		s := fmt.Sprintf("status %d %s", te.StatusCode, http.StatusText(te.StatusCode))
		var codes []string
		seen := map[string]bool{}
		for _, d := range te.Errors {
			code := string(d.Code)
			if code == "" || seen[code] {
				continue
			}
			seen[code] = true
			codes = append(codes, code)
		}
		if len(codes) > 0 {
			s += ": " + strings.Join(codes, ", ")
		}
		return redact.Strings(s, hide...)
	}
	return redact.Error(err, hide...)
}
