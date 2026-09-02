package k8s

import (
	"context"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abradner/hoist/pkg/image"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	// A documentation-range address and port: the tests assert it never reaches an error.
	server = "https://192.0.2.10:6443"
)

func pod(ns, name string, phase corev1.PodPhase, statuses, init []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     corev1.PodStatus{Phase: phase, ContainerStatuses: statuses, InitContainerStatuses: init},
	}
}

func status(name, imageID string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, ImageID: imageID}
}

func TestRunningImagesCountsRunningAndPendingPodsOnly(t *testing.T) {
	terminating := pod("app", "web-old", corev1.PodRunning, []corev1.ContainerStatus{status("web", "ghcr.io/example/web@"+digestC)}, nil)
	now := metav1.NewTime(time.Now())
	terminating.DeletionTimestamp = &now
	cs := fake.NewClientset(
		pod("app", "web-2", corev1.PodRunning,
			[]corev1.ContainerStatus{status("worker", "docker-pullable://ghcr.io/example/web@"+digestA), status("web", "ghcr.io/example/web@"+digestA)},
			[]corev1.ContainerStatus{status("dbwait", "ghcr.io/example/dbwait@"+digestB)}),
		pod("app", "web-1", corev1.PodPending,
			[]corev1.ContainerStatus{status("web", ""), status("sidecar", "sha256:"+strings.Repeat("d", 64))}, nil),
		pod("app", "purge-job", corev1.PodSucceeded, []corev1.ContainerStatus{status("purge", "ghcr.io/example/counta@"+digestC)}, nil),
		pod("app", "crashed", corev1.PodFailed, []corev1.ContainerStatus{status("web", "ghcr.io/example/web@"+digestC)}, nil),
		terminating,
		// Another namespace running the same repo on another digest must not be seen.
		pod("other", "web-9", corev1.PodRunning, []corev1.ContainerStatus{status("web", "ghcr.io/example/web@"+digestC)}, nil),
	)
	c := FromClientset(cs)
	got, err := c.RunningImages(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	want := []RunningImage{
		{Pod: "web-2", Container: "web", Ref: image.Ref{Repo: "ghcr.io/example/web", Digest: digestA}},
		{Pod: "web-2", Container: "worker", Ref: image.Ref{Repo: "ghcr.io/example/web", Digest: digestA}},
		{Pod: "web-2", Container: "dbwait", Init: true, Ref: image.Ref{Repo: "ghcr.io/example/dbwait", Digest: digestB}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RunningImages mismatch (-want +got):\n%s", diff)
	}
	for _, r := range got {
		if r.Ref.Digest == digestC {
			t.Errorf("a pod that must not count contributed %+v", r)
		}
	}
	// Scope: exactly one request, a list of pods in the namespace asked for, nothing else.
	actions := cs.Actions()
	if len(actions) != 1 || actions[0].GetVerb() != "list" || actions[0].GetResource().Resource != "pods" || actions[0].GetNamespace() != "app" {
		t.Errorf("actions = %v, want one list of pods in app", describeActions(actions))
	}
}

func describeActions(actions []k8stesting.Action) []string {
	var out []string
	for _, a := range actions {
		out = append(out, a.GetVerb()+" "+a.GetResource().Resource+" in "+a.GetNamespace())
	}
	return out
}

func TestRunningImagesRequiresNamespace(t *testing.T) {
	cs := fake.NewClientset()
	if _, err := FromClientset(cs).RunningImages(context.Background(), ""); err == nil {
		t.Fatal("empty namespace should be refused before any request")
	}
	if len(cs.Actions()) != 0 {
		t.Errorf("a request was made anyway: %v", describeActions(cs.Actions()))
	}
}

// The attacker here is the error path: a transport error from client-go carries the API
// server URL, and an API error carries the caller's identity. Neither may be printed.
func TestErrorsCarryNoServerAddress(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, &url.Error{Op: "Get", URL: server + "/api/v1/namespaces/app/pods", Err: errors.New("dial tcp 192.0.2.10:6443: connect: connection refused")}
	})
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "ghcr-pull", errors.New(`User "system:serviceaccount:x:y" cannot get resource`))
	})
	c := FromClientset(cs, server, "192.0.2.10:6443")

	_, err := c.RunningImages(context.Background(), "app")
	if err == nil {
		t.Fatal("expected the reactor's error")
	}
	for _, leak := range []string{"192.0.2.10", "6443", "https://"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
	if !strings.Contains(err.Error(), "listing pods in namespace app") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error lost the useful part: %v", err)
	}

	_, err = c.DockerConfigSecret(context.Background(), "app", "ghcr-pull")
	if err == nil {
		t.Fatal("expected the reactor's error")
	}
	if want := "cluster secret app/ghcr-pull: Forbidden"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestDockerConfigSecretYieldsKeychainAndNothingElse(t *testing.T) {
	const password = "ghp_FAKEsecret0000"
	cfg := `{"auths":{"ghcr.io":{"auth":"` + basic("robot", password) + `"},"https://index.docker.io/v1/":{"username":"hubuser","password":"hubpass"}}}`
	cs := fake.NewClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "ghcr-pull"},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(cfg)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "opaque"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"token": []byte(password)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "broken"},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths": ` + password)},
		},
	)
	c := FromClientset(cs)
	kc, err := c.DockerConfigSecret(context.Background(), "app", "ghcr-pull")
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]authn.AuthConfig{
		"ghcr.io":          {Username: "robot", Password: password},
		"index.docker.io":  {Username: "hubuser", Password: "hubpass"},
		"docker.io":        {Username: "hubuser", Password: "hubpass"},
		"registry.example": {},
	} {
		reg, err := name.NewRegistry(host)
		if err != nil {
			t.Fatal(err)
		}
		a, err := kc.Resolve(reg)
		if err != nil {
			t.Fatal(err)
		}
		if want == (authn.AuthConfig{}) {
			if a != authn.Anonymous {
				t.Errorf("%s: want Anonymous for an unknown host", host)
			}
			continue
		}
		got, err := a.Authorization()
		if err != nil {
			t.Fatal(err)
		}
		if got.Username != want.Username || got.Password != want.Password {
			t.Errorf("%s: got %s/%q, want %s/%q", host, got.Username, got.Password, want.Username, want.Password)
		}
	}
	// Scope: one get of the named secret in the named namespace.
	actions := cs.Actions()
	if len(actions) != 1 || actions[0].GetVerb() != "get" || actions[0].GetResource().Resource != "secrets" || actions[0].GetNamespace() != "app" {
		t.Errorf("actions = %v, want one get of secrets in app", describeActions(actions))
	}

	for sec, want := range map[string]string{
		"opaque":  "cluster secret app/opaque: type Opaque, want kubernetes.io/dockerconfigjson",
		"broken":  "cluster secret app/broken: .dockerconfigjson is not valid JSON",
		"missing": "cluster secret app/missing: NotFound",
	} {
		_, err := c.DockerConfigSecret(context.Background(), "app", sec)
		if err == nil {
			t.Errorf("%s: expected an error", sec)
			continue
		}
		if err.Error() != want {
			t.Errorf("%s: error = %q, want %q", sec, err, want)
		}
		if strings.Contains(err.Error(), password) {
			t.Errorf("%s: error carries the secret: %v", sec, err)
		}
	}
}

func basic(user, pass string) string {
	return base64Std(user + ":" + pass)
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestParseDockerConfigShapes(t *testing.T) {
	for label, tc := range map[string]struct{ in, wantErr string }{
		"empty":         {"", "no .dockerconfigjson data"},
		"bad base64":    {`{"auths":{"ghcr.io":{"auth":"***"}}}`, `.dockerconfigjson auths["ghcr.io"].auth is not base64`},
		"no colon":      {`{"auths":{"ghcr.io":{"auth":"` + base64Std("nocolon") + `"}}}`, `.dockerconfigjson auths["ghcr.io"].auth is not username:password`},
		"empty entries": {`{"auths":{"ghcr.io":{}}}`, ""},
	} {
		_, err := parseDockerConfig([]byte(tc.in))
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", label, err)
		case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
			t.Errorf("%s: error = %v, want %q", label, err, tc.wantErr)
		}
	}
}

func TestNormaliseHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://index.docker.io/v1/": "index.docker.io",
		"docker.io":                   "index.docker.io",
		"https://ghcr.io/":            "ghcr.io",
		"ghcr.io":                     "ghcr.io",
		"localhost:5000":              "localhost:5000",
	} {
		if got := normaliseHost(in); got != want {
			t.Errorf("normaliseHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// NewCluster reads the kubeconfig named by $KUBECONFIG, honours the named context, and
// reports which one it uses. The contexts here are placeholders (AGENTS.md §4.4).
func TestNewClusterSelectsContext(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	body := `apiVersion: v1
kind: Config
current-context: staging
clusters:
- name: staging
  cluster: {server: "` + server + `", insecure-skip-tls-verify: true}
- name: closed
  cluster: {server: "https://127.0.0.1:1"}
users:
- name: me
  user: {token: not-a-real-token}
contexts:
- name: staging
  context: {cluster: staging, user: me}
- name: closed-port
  context: {cluster: closed, user: me}
`
	if err := os.WriteFile(kubeconfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	if _, got, err := NewCluster(""); err != nil || got != "staging" {
		t.Errorf("NewCluster(\"\") = %q, %v; want the current context staging", got, err)
	}
	if _, got, err := NewCluster("closed-port"); err != nil || got != "closed-port" {
		t.Errorf("NewCluster(closed-port) = %q, %v", got, err)
	}
	_, _, err := NewCluster("nope")
	if err == nil || !strings.Contains(err.Error(), `kube context "nope" is not in the kubeconfig`) {
		t.Errorf("unknown context: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "192.0.2.10") {
		t.Errorf("error names the server: %v", err)
	}

	// A real client against a closed local port: the connection is refused, and the error
	// must say so without the address. Nothing listens on port 1.
	c, _, err := NewCluster("closed-port")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = c.RunningImages(ctx, "app")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), ":1/") || strings.Contains(err.Error(), "https://") {
		t.Errorf("error names the server: %v", err)
	}
	if !strings.Contains(err.Error(), "listing pods in namespace app") {
		t.Errorf("error lost its context: %v", err)
	}
}

func TestNewClusterWithoutCurrentContextAsksForOne(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	body := `apiVersion: v1
kind: Config
clusters:
- name: staging
  cluster: {server: "` + server + `"}
users:
- name: me
  user: {token: not-a-real-token}
contexts:
- name: staging
  context: {cluster: staging, user: me}
`
	if err := os.WriteFile(kubeconfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	_, _, err := NewCluster("")
	if err == nil || !strings.Contains(err.Error(), "--kube-context") {
		t.Errorf("want an error asking for --kube-context, got %v", err)
	}
}
