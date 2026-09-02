package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/abradner/hoist/pkg/image"
	"github.com/abradner/hoist/pkg/k8s"
)

// gate is the adversary's registry front door: Basic auth, a user that authenticates but
// lacks scope (AGENTS.md §6.1 items 1 and 2), and error bodies that echo the credential
// they rejected — which the client must never repeat.
type gate struct {
	inner    http.Handler
	users    map[string]string // user → password
	denied   map[string]bool   // users that authenticate but get 403 on everything
	pingFail bool              // answer every request 500 with a secret in the body

	mu       sync.Mutex
	attempts map[string]int // per user, "" for anonymous
}

func (g *gate) count(user string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.attempts == nil {
		g.attempts = map[string]int{}
	}
	g.attempts[user]++
}

func (g *gate) tries(user string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.attempts[user]
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.pingFail {
		writeErr(w, http.StatusInternalServerError, "UNKNOWN", "backend token BODY-SECRET-7f3a exposed")
		return
	}
	user, pass, ok := r.BasicAuth()
	g.count(user)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	if g.users[user] != pass {
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "credential "+pass+" for "+user+" rejected")
		return
	}
	if g.denied[user] {
		writeErr(w, http.StatusForbidden, "DENIED", "user "+user+" with credential "+pass+" lacks read:packages")
		return
	}
	g.inner.ServeHTTP(w, r)
}

// rewrite sends every request for ghcr.io to the test server, so references in tests can
// be real ghcr.io ones (the env source is scoped to that host) without any request
// leaving the process.
type rewrite struct{ host string }

func (rw rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = rw.host
	req.Host = rw.host
	return http.DefaultTransport.RoundTrip(req)
}

// testRegistry is one in-memory registry behind two doors: open (for the test to push
// through) and gated (what the Client talks to, as ghcr.io).
type testRegistry struct {
	gate      *gate
	open      string // host:port
	transport http.RoundTripper
}

func newTestRegistry(t *testing.T) *testRegistry {
	t.Helper()
	inner := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	g := &gate{inner: inner, users: map[string]string{"scoped": "pw-scoped", "good": "pw-good"}, denied: map[string]bool{"scoped": true}}
	open := httptest.NewServer(inner)
	gated := httptest.NewServer(g)
	t.Cleanup(open.Close)
	t.Cleanup(gated.Close)
	return &testRegistry{gate: g, open: strings.TrimPrefix(open.URL, "http://"), transport: rewrite{host: strings.TrimPrefix(gated.URL, "http://")}}
}

func (tr *testRegistry) pushImage(t *testing.T, repoTag string) v1.Hash {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(tr.open + "/" + repoTag)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	h, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (tr *testRegistry) pushIndex(t *testing.T, repoTag string) (index v1.Hash, children []v1.Hash) {
	t.Helper()
	idx, err := random.Index(256, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(tr.open + "/" + repoTag)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	index, err = idx.Digest()
	if err != nil {
		t.Fatal(err)
	}
	m, err := idx.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range m.Manifests {
		children = append(children, d.Digest)
	}
	return index, children
}

// noEnv clears the env source so a developer's own token cannot leak into a test.
func noEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"HOIST_GHCR_TOKEN", "GHCR_TOKEN", "HOIST_GHCR_USER", "GHCR_USER"} {
		t.Setenv(v, "")
	}
}

// neverOp makes any op invocation a test failure; every test installs it unless it is
// testing the op source itself.
func neverOp(t *testing.T) {
	t.Helper()
	orig := opRead
	opRead = func(context.Context, string) (string, error) {
		t.Error("op was executed without an OpRef")
		return "", errors.New("op must not run")
	}
	t.Cleanup(func() { opRead = orig })
}

func newClient(t *testing.T, tr *testRegistry, cfg AuthConfig) *Client {
	t.Helper()
	cfg.Transport = tr.transport
	if cfg.Keychain == nil {
		cfg.Keychain = anonKeychain{} // no entry for any host, like an empty ~/.docker/config.json
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// anonKeychain has no entries.
type anonKeychain struct{}

func (anonKeychain) Resolve(authn.Resource) (authn.Authenticator, error) { return authn.Anonymous, nil }

func mustRef(t *testing.T, s string) image.Ref {
	t.Helper()
	r, err := image.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHeadSinglePlatformReturnsManifestDigest(t *testing.T) {
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	want := tr.pushImage(t, "example/app:v1")
	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthEnv}})
	t.Setenv("HOIST_GHCR_TOKEN", "pw-good")
	t.Setenv("HOIST_GHCR_USER", "good")
	got, err := c.Head(context.Background(), mustRef(t, "ghcr.io/example/app:v1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want.String() {
		t.Errorf("Head = %s, want %s", got, want)
	}
	if used := c.AuthSourceUsed(); used != "env (HOIST_GHCR_TOKEN)" {
		t.Errorf("AuthSourceUsed = %q", used)
	}
	// By digest: confirmed and returned as is.
	got, err = c.Head(context.Background(), image.Ref{Repo: "ghcr.io/example/app", Digest: want.String()})
	if err != nil || got != want.String() {
		t.Errorf("Head by digest = %s, %v", got, err)
	}
}

// The digest a pull by tag pins for a multi-arch image is the index digest — what the
// runtime records in imageID — never one platform manifest's. This test freezes the choice
// documented on the package.
func TestHeadMultiArchReturnsIndexDigest(t *testing.T) {
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	index, children := tr.pushIndex(t, "example/multi:sha-abc123")
	if len(children) != 2 {
		t.Fatalf("index has %d children, want 2", len(children))
	}
	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthKeychain}, Keychain: k8s.StaticKeychain{Username: "good", Password: "pw-good"}})
	got, err := c.Head(context.Background(), mustRef(t, "ghcr.io/example/multi:sha-abc123"))
	if err != nil {
		t.Fatal(err)
	}
	if got != index.String() {
		t.Errorf("Head = %s, want the index digest %s", got, index)
	}
	for _, ch := range children {
		if got == ch.String() {
			t.Errorf("Head returned a platform manifest digest %s", ch)
		}
	}
}

func TestTagsSorted(t *testing.T) {
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	for _, tag := range []string{"v2", "latest", "sha-1", "v1"} {
		tr.pushImage(t, "example/app:"+tag)
	}
	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthKeychain}, Keychain: k8s.StaticKeychain{Username: "good", Password: "pw-good"}})
	got, err := c.Tags(context.Background(), "ghcr.io/example/app")
	if err != nil {
		t.Fatal(err)
	}
	if want := "latest,sha-1,v1,v2"; strings.Join(got, ",") != want {
		t.Errorf("Tags = %v, want %s", got, want)
	}
	if c.AuthSourceUsed() != "keychain" {
		t.Errorf("AuthSourceUsed = %q", c.AuthSourceUsed())
	}
}

// The chain in the brief's own adversary: env holds a wrong token, the keychain entry
// authenticates but lacks scope, the cluster pull secret works. The winner is reported
// by name and remembered — the losers are not retried on the next call.
func TestChainReportsWinnerAndRemembersIt(t *testing.T) {
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	want := tr.pushImage(t, "example/app:v1")
	t.Setenv("GHCR_TOKEN", "pw-wrong")
	cluster := &k8s.Fake{Secrets: map[string]authn.Keychain{"app-staging/ghcr-pull": k8s.StaticKeychain{Username: "good", Password: "pw-good"}}}
	c := newClient(t, tr, AuthConfig{
		Order:         DefaultAuthOrder,
		Keychain:      k8s.StaticKeychain{Username: "scoped", Password: "pw-scoped"},
		ClusterSecret: "app-staging/ghcr-pull",
		Cluster:       cluster,
	})
	got, err := c.Head(context.Background(), mustRef(t, "ghcr.io/example/app:v1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want.String() {
		t.Errorf("Head = %s, want %s", got, want)
	}
	if used := c.AuthSourceUsed(); used != "cluster secret app-staging/ghcr-pull" {
		t.Errorf("AuthSourceUsed = %q, want the cluster secret", used)
	}
	if tr.gate.tries("hoist") == 0 || tr.gate.tries("scoped") == 0 {
		t.Fatalf("control: the losing links were never tried (attempts %v)", tr.gate.attempts)
	}
	envTries, scopedTries := tr.gate.tries("hoist"), tr.gate.tries("scoped")
	if _, err := c.Tags(context.Background(), "ghcr.io/example/app"); err != nil {
		t.Fatal(err)
	}
	if tr.gate.tries("hoist") != envTries || tr.gate.tries("scoped") != scopedTries {
		t.Errorf("losing links were retried after a winner was found: %v", tr.gate.attempts)
	}
	if calls := strings.Join(cluster.Calls, ","); calls != "DockerConfigSecret app-staging/ghcr-pull" {
		t.Errorf("cluster secret read %q, want exactly once", calls)
	}
}

// Every link fails; the error names each link's outcome and nothing the registry echoed.
func TestChainFailureNamesLinksAndCarriesNoSecret(t *testing.T) {
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	tr.pushImage(t, "example/app:v1")
	t.Setenv("HOIST_GHCR_TOKEN", "pw-wrong-env")
	t.Setenv("HOIST_GHCR_USER", "envuser")
	c := newClient(t, tr, AuthConfig{Order: DefaultAuthOrder, Keychain: k8s.StaticKeychain{Username: "scoped", Password: "pw-scoped"}})
	_, err := c.Tags(context.Background(), "ghcr.io/example/app")
	if err == nil {
		t.Fatal("expected the chain to fail")
	}
	msg := err.Error()
	for _, want := range []string{
		"no credential source worked for ghcr.io",
		"env (HOIST_GHCR_TOKEN): status 401 Unauthorized: UNAUTHORIZED",
		"keychain: status 403 Forbidden: DENIED",
		"cluster: not configured",
		"op: not configured",
		"anonymous: status 401 Unauthorized: UNAUTHORIZED",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	for _, leak := range []string{"pw-wrong-env", "envuser", "pw-scoped", "scoped", "rejected", "lacks read:packages", "authentication required"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error carries %q:\n%s", leak, msg)
		}
	}
	if c.AuthSourceUsed() != "" {
		t.Errorf("AuthSourceUsed = %q after total failure", c.AuthSourceUsed())
	}
}

func TestResponseBodyNeverReachesHeadError(t *testing.T) {
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	tr.gate.pingFail = true
	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthKeychain}, Keychain: k8s.StaticKeychain{Username: "good", Password: "pw-good"}})
	_, err := c.Head(context.Background(), mustRef(t, "ghcr.io/example/app:v1"))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), "BODY-SECRET") || strings.Contains(err.Error(), "backend token") {
		t.Errorf("error repeats the response body: %v", err)
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("error lost the status: %v", err)
	}
}

// A credential that is accepted but finds no such tag is not an auth failure: the chain
// stops there and says which link it was using.
func TestNonAuthFailureStopsTheChain(t *testing.T) {
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	tr.pushImage(t, "example/app:v1")
	t.Setenv("GHCR_TOKEN", "pw-good")
	t.Setenv("GHCR_USER", "good")
	c := newClient(t, tr, AuthConfig{Order: DefaultAuthOrder, Keychain: k8s.StaticKeychain{Username: "scoped", Password: "pw-scoped"}})
	_, err := c.Head(context.Background(), mustRef(t, "ghcr.io/example/app:v9"))
	if err == nil {
		t.Fatal("expected a failure for a missing tag")
	}
	// A HEAD response has no body, so no error code: the status is all there is.
	if !strings.Contains(err.Error(), "status 404 Not Found (auth: env (GHCR_TOKEN))") {
		t.Errorf("error = %v", err)
	}
	if tr.gate.tries("scoped") != 0 {
		t.Errorf("the keychain link was tried after a non-auth failure: %v", tr.gate.attempts)
	}
}

func TestOpRunsOnlyWhenConfigured(t *testing.T) {
	noEnv(t)
	tr := newTestRegistry(t)
	want := tr.pushImage(t, "example/app:v1")
	var gotRef string
	orig := opRead
	opRead = func(_ context.Context, ref string) (string, error) {
		gotRef = ref
		return "pw-good\n", nil
	}
	t.Cleanup(func() { opRead = orig })
	t.Setenv("GHCR_USER", "good")

	// op listed but not configured: skipped, and the program is never run.
	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthOp}})
	_, err := c.Head(context.Background(), mustRef(t, "ghcr.io/example/app:v1"))
	if err == nil || !strings.Contains(err.Error(), "op: not configured") {
		t.Errorf("unconfigured op: %v", err)
	}
	if gotRef != "" {
		t.Fatalf("op ran without an OpRef (ref %q)", gotRef)
	}

	c = newClient(t, tr, AuthConfig{Order: []AuthSource{AuthOp}, OpRef: "op://vault/item/field"})
	got, err := c.Head(context.Background(), mustRef(t, "ghcr.io/example/app:v1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want.String() || c.AuthSourceUsed() != "op" || gotRef != "op://vault/item/field" {
		t.Errorf("Head = %s, used %q, ref %q", got, c.AuthSourceUsed(), gotRef)
	}
	// The reference is configuration the operator redacts elsewhere; it is not in the report.
	if strings.Contains(c.AuthSourceUsed(), "op://") {
		t.Error("AuthSourceUsed repeats the op reference")
	}
}

func TestOpFailureIsReportedByExitOnly(t *testing.T) {
	noEnv(t)
	tr := newTestRegistry(t)
	orig := opRead
	opRead = func(context.Context, string) (string, error) { return "", errors.New("op read failed (exit 1)") }
	t.Cleanup(func() { opRead = orig })
	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthOp}, OpRef: "op://vault/item/field"})
	_, err := c.Head(context.Background(), mustRef(t, "ghcr.io/example/app:v1"))
	if err == nil || !strings.Contains(err.Error(), "op: op read failed (exit 1)") || strings.Contains(err.Error(), "op://") {
		t.Errorf("error = %v", err)
	}
}

func TestEnvSourceIsScopedToGHCR(t *testing.T) {
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	t.Setenv("GHCR_TOKEN", "pw-good")
	c := newClient(t, tr, AuthConfig{Order: []AuthSource{AuthEnv}})
	_, err := c.Head(context.Background(), mustRef(t, "registry.example/app:v1"))
	if err == nil || !strings.Contains(err.Error(), "env: only for ghcr.io, not registry.example") {
		t.Errorf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "anonymous:") {
		t.Errorf("the anonymous attempt was not reported: %v", err)
	}
}

// The real DefaultKeychain reads ~/.docker/config.json; pointed at a temp HOME and
// DOCKER_CONFIG it must find the entry there and nothing of the developer's.
func TestDefaultKeychainReadsDockerConfig(t *testing.T) {
	noEnv(t)
	neverOp(t)
	tr := newTestRegistry(t)
	want := tr.pushImage(t, "example/app:v1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DOCKER_CONFIG", home)
	cfg := `{"auths":{"ghcr.io":{"auth":"` + basicAuth("good", "pw-good") + `"}}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(AuthConfig{Order: []AuthSource{AuthKeychain}, Transport: tr.transport})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Head(context.Background(), mustRef(t, "ghcr.io/example/app:v1"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want.String() || c.AuthSourceUsed() != "keychain" {
		t.Errorf("Head = %s, used %q", got, c.AuthSourceUsed())
	}
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

func TestNewValidatesConfig(t *testing.T) {
	for label, cfg := range map[string]AuthConfig{
		"unknown source":            {Order: []AuthSource{"vault"}},
		"duplicate source":          {Order: []AuthSource{AuthEnv, AuthEnv}},
		"cluster secret shape":      {ClusterSecret: "no-slash", Cluster: &k8s.Fake{}},
		"cluster secret no name":    {ClusterSecret: "ns/", Cluster: &k8s.Fake{}},
		"cluster secret no cluster": {ClusterSecret: "ns/name"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted %+v", label, cfg)
		}
	}
	c, err := New(AuthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sourceNames(c.cfg.Order), ",") != "env,keychain,cluster,op" {
		t.Errorf("default order = %v", c.cfg.Order)
	}
}

func sourceNames(order []AuthSource) []string {
	out := make([]string, 0, len(order))
	for _, s := range order {
		out = append(out, string(s))
	}
	return out
}

func TestParseAuthOrder(t *testing.T) {
	got, err := ParseAuthOrder([]string{"cluster", " env"})
	if err != nil || len(got) != 2 || got[0] != AuthCluster || got[1] != AuthEnv {
		t.Errorf("ParseAuthOrder = %v, %v", got, err)
	}
	for _, bad := range [][]string{{"vault"}, {"env", "env"}} {
		if _, err := ParseAuthOrder(bad); err == nil {
			t.Errorf("ParseAuthOrder(%v) accepted", bad)
		}
	}
}
