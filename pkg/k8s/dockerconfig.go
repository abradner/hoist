package k8s

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
)

// dockerConfig is the subset of ~/.docker/config.json that a pull secret carries. It is
// parsed here rather than through docker/cli so that no extra direct dependency is taken
// for thirty lines (AGENTS.md §4.7); the format is stable and documented.
type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Auth     string `json:"auth"` // base64(username:password)
	Username string `json:"username"`
	Password string `json:"password"`
}

// secretKeychain is an authn.Keychain over one parsed pull secret. It holds the
// credentials and is the only form in which they leave this package.
type secretKeychain struct {
	auths map[string]authn.AuthConfig // keyed by normalised registry host
}

// parseDockerConfig decodes a .dockerconfigjson payload. Errors describe the shape of the
// problem and never quote the payload.
func parseDockerConfig(data []byte) (authn.Keychain, error) {
	if len(data) == 0 {
		return nil, errors.New("no .dockerconfigjson data")
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, errors.New(".dockerconfigjson is not valid JSON")
	}
	kc := &secretKeychain{auths: map[string]authn.AuthConfig{}}
	for host, a := range cfg.Auths {
		user, pass := a.Username, a.Password
		if a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return nil, fmt.Errorf(".dockerconfigjson auths[%q].auth is not base64", host)
			}
			u, p, ok := strings.Cut(string(raw), ":")
			if !ok {
				return nil, fmt.Errorf(".dockerconfigjson auths[%q].auth is not username:password", host)
			}
			user, pass = u, p
		}
		if user == "" && pass == "" {
			continue
		}
		kc.auths[normaliseHost(host)] = authn.AuthConfig{Username: user, Password: pass}
	}
	return kc, nil
}

// Resolve implements authn.Keychain: the entry for the target's registry, else Anonymous
// (the convention every keychain follows, so "no entry" is not an error).
func (k *secretKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	if a, ok := k.auths[normaliseHost(target.RegistryStr())]; ok {
		return authn.FromConfig(a), nil
	}
	return authn.Anonymous, nil
}

// normaliseHost reduces the ways a docker config spells a registry — "https://ghcr.io",
// "https://index.docker.io/v1/", "docker.io" — to one comparable host.
func normaliseHost(h string) string {
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimSuffix(h, "/v1/")
	h = strings.TrimSuffix(h, "/v1")
	h = strings.TrimSuffix(h, "/")
	if h == "docker.io" || h == "registry-1.docker.io" {
		h = "index.docker.io"
	}
	return h
}
