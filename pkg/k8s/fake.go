package k8s

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
)

// Fake is an in-memory Cluster for tests in other packages. Images is keyed by namespace;
// Secrets by "namespace/name". Err, when set, is returned by every call. Calls records
// every method invocation as "RunningImages <ns>" or "DockerConfigSecret <ns>/<name>" so a
// test can assert scope (nothing outside the namespace it named) and count.
type Fake struct {
	Images  map[string][]RunningImage
	Secrets map[string]authn.Keychain
	Err     error
	Calls   []string
}

// RunningImages implements Cluster.
func (f *Fake) RunningImages(_ context.Context, namespace string) ([]RunningImage, error) {
	f.Calls = append(f.Calls, "RunningImages "+namespace)
	if f.Err != nil {
		return nil, f.Err
	}
	return append([]RunningImage(nil), f.Images[namespace]...), nil
}

// DockerConfigSecret implements Cluster.
func (f *Fake) DockerConfigSecret(_ context.Context, namespace, name string) (authn.Keychain, error) {
	key := namespace + "/" + name
	f.Calls = append(f.Calls, "DockerConfigSecret "+key)
	if f.Err != nil {
		return nil, f.Err
	}
	kc, ok := f.Secrets[key]
	if !ok {
		return nil, fmt.Errorf("cluster secret %s: NotFound", key)
	}
	return kc, nil
}

// StaticKeychain is an authn.Keychain that answers every registry with one credential;
// for tests that need a secret without a cluster.
type StaticKeychain struct{ Username, Password string }

// Resolve implements authn.Keychain.
func (s StaticKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return &authn.Basic{Username: s.Username, Password: s.Password}, nil
}
