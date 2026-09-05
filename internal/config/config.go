package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole file. File and Found are set by Load, never read from YAML.
type Config struct {
	Repos       []RepoConfig      `yaml:"repos,omitempty"`
	Registries  []RegistryConfig  `yaml:"registries,omitempty"`
	Poll        PollConfig        `yaml:"poll"`
	Preferences PreferencesConfig `yaml:"preferences"`

	// File is the path Load read, or looked for. Found reports whether it existed.
	File  string `yaml:"-"`
	Found bool   `yaml:"-"`
}

// PreferencesConfig is operator-facing UX behavior — how hoist itself behaves for this
// operator, as distinct from Repos/Registries/Poll's own promotion-pipeline policy. Unlike
// those, every field here has a purely local, no-network-effect default that's safe to change
// on a whim; nothing here is checked against a repo or forge.
type PreferencesConfig struct {
	// OpenPR controls what pressing o on the flight screen does with a promotion's PR URL
	// (flight.OpenPRMsg, cmd/hoist/wiring.go): "launch" attempts to open it in a browser and
	// stays silent on success (M4's original behavior, for a desktop session with one to
	// open into); "display" only ever shows the URL as text (for a headless/SSH session with
	// no browser to launch into at all — see AGENTS.md's own note on this); "both" attempts
	// the launch AND always shows the URL as text regardless of outcome, so a copy/paste
	// fallback exists even on a desktop session where launching usually just works. Default
	// "both": it never regresses the desktop case (launch is still attempted) and never
	// leaves a headless session with nothing to act on.
	OpenPR string `yaml:"open_pr"`
	// BrowserLaunchTimeout bounds one "launch" attempt (cmd/hoist/wiring.go's runLauncher) —
	// see its own doc comment for why this bounds the LAUNCHER's exit, not the browser
	// window's own lifetime, and is safe to leave short. Default 5s.
	BrowserLaunchTimeout Duration `yaml:"browser_launch_timeout"`
}

// RepoConfig describes one GitOps repository. Only Path is ever required, and only when
// the CLI has to choose a checkout without --repo; everything else has a default or is
// consumed by a later milestone (noted per field).
type RepoConfig struct {
	Name          string            `yaml:"name,omitempty"`       // label; defaults to the basename of Path
	Path          string            `yaml:"path,omitempty"`       // checkout, as written (~ allowed); see Dir
	GitHub        string            `yaml:"github,omitempty"`     // owner/name; M3
	AppsRoot      string            `yaml:"apps_root"`            // default cluster/apps
	Promotable    []string          `yaml:"promotable,omitempty"` // image repo prefixes; replaces the CLI placeholder when set
	Envs          EnvsConfig        `yaml:"envs"`
	Approvers     []string          `yaml:"approvers,omitempty"`     // M4
	Collaborators bool              `yaml:"collaborators,omitempty"` // M4: also accept a write-permission collaborator, per Forge.IsAllowedAuthor
	CI            CIConfig          `yaml:"ci"`                      // M4
	Kube          KubeConfig        `yaml:"kube,omitempty"`          // M2
	DigestSources []string          `yaml:"digest_sources"`          // M2; default pods, manifest, registry
	Apps          map[string]string `yaml:"apps,omitempty"`          // image repo -> app git repo; M7

	// Dir is Path with ~ expanded and cleaned; Key is this entry's YAML path (repos[N]).
	// Both are derived by Normalize.
	Dir string `yaml:"-"`
	Key string `yaml:"-"`
}

// EnvsConfig names the envs that matter to policy. Env names are not checked against the
// repo here — Load never reads the repo (see doc.go); a name that no Application wrapper
// declares is reported by the consumer that discovered the repo.
type EnvsConfig struct {
	Production []string          `yaml:"production,omitempty"`
	Pairs      map[string]string `yaml:"pairs,omitempty"`    // source env -> target env
	Approval   map[string]string `yaml:"approval,omitempty"` // env -> comment|auto
}

// CIConfig is how a PR with no check-runs is treated (M4).
type CIConfig struct {
	None  string   `yaml:"none"`  // green|prompt|block; default green
	Grace Duration `yaml:"grace"` // how long to wait for checks to appear; default 3m
}

// KubeConfig names the kubeconfig context hoist reads pods from (M2), and, from M5, where
// Argo CD's own Application custom resources live on that cluster.
type KubeConfig struct {
	Context string `yaml:"context,omitempty"`
	// ArgoNamespace is the namespace Argo CD Application custom resources live in — the
	// control-plane namespace (conventionally "argocd"), which is a different thing from
	// spec.destination.namespace (the workload's own target env: see gitops.Env's doc
	// comment, and every family's Application wrapper in this repo's own fixtures, which sets
	// metadata.namespace: argocd distinctly from spec.destination.namespace). pkg/argo's
	// invariant 1 requires this be confirmed from config rather than assumed a fixed
	// "argocd" — Normalize fills the "argocd" default so it need not be spelled out in every
	// config file, but the value driving pkg/argo always came from here, never a hardcoded
	// literal in pkg/argo itself.
	ArgoNamespace string `yaml:"argo_namespace,omitempty"`
}

// RegistryConfig is the credential chain for one image repo prefix.
type RegistryConfig struct {
	Prefix  string        `yaml:"prefix"`
	Auth    []string      `yaml:"auth"` // order tried; default env, keychain, cluster, op
	Cluster ClusterSecret `yaml:"cluster"`
	Op      string        `yaml:"op,omitempty"` // op://vault/item/field; redacted by Redacted
}

// ClusterSecret is the pull secret the cluster auth source reads. Opt-in: both fields or
// neither.
type ClusterSecret struct {
	Namespace string `yaml:"namespace,omitempty"`
	Secret    string `yaml:"secret,omitempty"`
}

// PollConfig is how often the engine re-observes each remote (M4/M5).
type PollConfig struct {
	CI       Duration `yaml:"ci"`
	Approval Duration `yaml:"approval"`
	Argo     Duration `yaml:"argo"`
	Rollout  Duration `yaml:"rollout"`
	Deadline Duration `yaml:"deadline"`
}

// Duration is a time.Duration that reads and writes time.ParseDuration syntax ("20s",
// "4h"). A bare number is refused: yaml.v3 would otherwise read it as nanoseconds.
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalYAML renders the duration as its string form.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML parses a string scalar with time.ParseDuration.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return fmt.Errorf("line %d: duration must be a quoted-or-plain string like 30s or 4h, got %s", n.Line, n.Value)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// Defaults for the optional knobs, applied by Normalize.
const (
	DefaultAppsRoot      = "cluster/apps"
	DefaultCINone        = "green"
	DefaultCIGrace       = Duration(3 * time.Minute)
	DefaultArgoNamespace = "argocd"

	ApprovalComment = "comment"
	ApprovalAuto    = "auto"

	// OpenPRLaunch, OpenPRDisplay, OpenPRBoth are PreferencesConfig.OpenPR's allowed values —
	// see its own doc comment for what each means.
	OpenPRLaunch  = "launch"
	OpenPRDisplay = "display"
	OpenPRBoth    = "both"

	DefaultOpenPR               = OpenPRBoth
	DefaultBrowserLaunchTimeout = Duration(5 * time.Second)
)

var (
	defaultDigestSources = []string{"pods", "manifest", "registry"}
	defaultAuth          = []string{"env", "keychain", "cluster", "op"}
	defaultPoll          = PollConfig{
		CI:       Duration(20 * time.Second),
		Approval: Duration(30 * time.Second),
		Argo:     Duration(5 * time.Second),
		Rollout:  Duration(3 * time.Second),
		Deadline: Duration(4 * time.Hour),
	}
)

// ErrUnknownRepo is wrapped by Repo when a selector names no configured repo, so the CLI
// can tell "not in the file" (fall back to treating it as a path) from "ambiguous".
var ErrUnknownRepo = errors.New("unknown repo")

// DefaultPath is where Load looks when --config is not given: $XDG_CONFIG_HOME/hoist/
// config.yaml, else ~/.config/hoist/config.yaml. The XDG rule applies on every platform.
func DefaultPath() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "hoist", "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating config: %w", err)
	}
	return filepath.Join(home, ".config", "hoist", "config.yaml"), nil
}

// Load reads, normalizes and validates the file at path. A missing file yields the
// defaults with Found false and no error; any other failure — unreadable, malformed,
// unknown key, invalid value — is an error naming the file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c := &Config{File: path}
		if err := c.Normalize(); err != nil {
			return nil, err
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	c, err := Parse(data, path)
	if err != nil {
		return nil, err
	}
	c.Found = true
	return c, nil
}

// Parse decodes data as the file named file (for messages), then Normalize and Validate.
// Only one YAML document is accepted: a second `---` document in the file is a mistake
// (the rest of the file is silently ignored by a decoder that only reads the first), not
// an alternate config, so it is rejected rather than dropped.
func Parse(data []byte, file string) (*Config, error) {
	c := &Config{File: file}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) { // io.EOF: an empty file is an empty config
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("%s: multiple YAML documents", file)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := c.Normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Normalize fills every documented default and derives Dir, Name and Key. It is the one
// place defaults live; Validate and every consumer see the filled-in value.
func (c *Config) Normalize() error {
	for i := range c.Repos {
		r := &c.Repos[i]
		r.Key = fmt.Sprintf("repos[%d]", i)
		if r.Path != "" {
			dir, err := expandHome(r.Path)
			if err != nil {
				return fmt.Errorf("%s.path: %w", r.Key, err)
			}
			r.Dir = dir
		}
		if r.Name == "" && r.Dir != "" {
			r.Name = filepath.Base(r.Dir)
		}
		if r.AppsRoot == "" {
			r.AppsRoot = DefaultAppsRoot
		}
		if r.CI.None == "" {
			r.CI.None = DefaultCINone
		}
		if r.CI.Grace == 0 {
			r.CI.Grace = DefaultCIGrace
		}
		if r.DigestSources == nil {
			r.DigestSources = append([]string(nil), defaultDigestSources...)
		}
		if r.Kube.ArgoNamespace == "" {
			r.Kube.ArgoNamespace = DefaultArgoNamespace
		}
		// §4.5: a production env is gated by the magic comment unless the file says
		// otherwise for that env by name. Non-production envs default to auto and are
		// answered by Approval, since their names are not known at load time.
		for _, env := range r.Envs.Production {
			if _, ok := r.Envs.Approval[env]; !ok && env != "" {
				if r.Envs.Approval == nil {
					r.Envs.Approval = map[string]string{}
				}
				r.Envs.Approval[env] = ApprovalComment
			}
		}
	}
	for i := range c.Registries {
		if c.Registries[i].Auth == nil {
			c.Registries[i].Auth = append([]string(nil), defaultAuth...)
		}
	}
	fill := func(d *Duration, def Duration) {
		if *d == 0 {
			*d = def
		}
	}
	fill(&c.Poll.CI, defaultPoll.CI)
	fill(&c.Poll.Approval, defaultPoll.Approval)
	fill(&c.Poll.Argo, defaultPoll.Argo)
	fill(&c.Poll.Rollout, defaultPoll.Rollout)
	fill(&c.Poll.Deadline, defaultPoll.Deadline)
	if c.Preferences.OpenPR == "" {
		c.Preferences.OpenPR = DefaultOpenPR
	}
	fill(&c.Preferences.BrowserLaunchTimeout, DefaultBrowserLaunchTimeout)
	return nil
}

// expandHome turns ~ and ~/x into the user's home directory; ~user is not supported.
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Clean(p), nil
}

// IsProduction reports whether env is one the operator listed as production. The single
// implementation of that question: envs.production is the one config authority that governs
// PR-required, approval-required and the direct-mode refusal alike (AGENTS.md §4.5), and a
// second copy of the scan is how those three drift apart.
func (e EnvsConfig) IsProduction(env string) bool {
	for _, p := range e.Production {
		if p == env {
			return true
		}
	}
	return false
}

// Approval is the approval mode for env: the explicit setting, else comment for a
// production env, else auto.
func (r RepoConfig) Approval(env string) string {
	if v, ok := r.Envs.Approval[env]; ok {
		return v
	}
	if r.Envs.IsProduction(env) {
		return ApprovalComment
	}
	return ApprovalAuto
}

// problems collects validation errors, each prefixed with the file and YAML path.
type problems struct {
	file string
	errs []error
}

func (p *problems) add(path, format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf("%s: %s: %s", p.file, path, fmt.Sprintf(format, args...)))
}

// Validate checks every value the file may carry and reports all problems at once. It
// assumes Normalize has run: an empty list here is one the file spelled out as empty.
func (c *Config) Validate() error {
	p := &problems{file: c.File}
	names := map[string]string{}
	dirs := map[string]string{}
	for _, r := range c.Repos {
		validateRepo(p, r)
		if r.Name != "" {
			if prev, dup := names[r.Name]; dup {
				p.add(r.Key+".name", "%q is also %s", r.Name, prev)
			} else {
				names[r.Name] = r.Key
			}
		}
		if r.Dir != "" {
			if prev, dup := dirs[r.Dir]; dup {
				p.add(r.Key+".path", "%q is also %s", r.Path, prev)
			} else {
				dirs[r.Dir] = r.Key
			}
		}
	}
	for i, reg := range c.Registries {
		validateRegistry(p, fmt.Sprintf("registries[%d]", i), reg)
	}
	for name, d := range map[string]Duration{"ci": c.Poll.CI, "approval": c.Poll.Approval, "argo": c.Poll.Argo, "rollout": c.Poll.Rollout, "deadline": c.Poll.Deadline} {
		if d <= 0 {
			p.add("poll."+name, "must be a positive duration, got %s", d)
		}
	}
	validateEnum(p, "preferences.open_pr", c.Preferences.OpenPR, OpenPRLaunch, OpenPRDisplay, OpenPRBoth)
	if c.Preferences.BrowserLaunchTimeout <= 0 {
		p.add("preferences.browser_launch_timeout", "must be a positive duration, got %s", c.Preferences.BrowserLaunchTimeout)
	}
	return errors.Join(sortedErrs(p.errs)...)
}

func validateRepo(p *problems, r RepoConfig) {
	k := r.Key
	if r.Path == "" && r.Name == "" {
		p.add(k, "needs a name or a path")
	}
	if r.GitHub != "" {
		if owner, name, ok := strings.Cut(r.GitHub, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			p.add(k+".github", "want owner/name, got %q", r.GitHub)
		}
	}
	if r.Promotable != nil {
		if len(r.Promotable) == 0 {
			p.add(k+".promotable", "must not be empty; omit it to use the placeholder default, or list your registry prefixes")
		}
		for i, pre := range r.Promotable {
			if strings.TrimSpace(pre) == "" {
				p.add(fmt.Sprintf("%s.promotable[%d]", k, i), "empty prefix")
			}
		}
	}
	// Normalize already defaults an omitted or explicitly-empty value to "argocd" (a plain
	// YAML string can't distinguish the two — unlike DigestSources/Promotable, which are
	// slices and so can) — this call always runs, on the default "argocd" exactly as much as
	// on a genuinely user-supplied value; checkNoSurroundingWhitespace itself is simply a
	// no-op on both an empty string and an already-unpadded one like "argocd", so only a
	// genuinely padded value (user-supplied, since Normalize's own default is never padded)
	// ever actually triggers a validation error here.
	checkNoSurroundingWhitespace(p, k+".kube.argo_namespace", r.Kube.ArgoNamespace)
	validateEnvs(p, k+".envs", r.Envs)
	for i, a := range r.Approvers {
		if strings.TrimSpace(a) == "" {
			p.add(fmt.Sprintf("%s.approvers[%d]", k, i), "empty login")
		}
	}
	validateEnum(p, k+".ci.none", r.CI.None, "green", "prompt", "block")
	if r.CI.Grace <= 0 {
		p.add(k+".ci.grace", "must be a positive duration, got %s", r.CI.Grace)
	}
	validateList(p, k+".digest_sources", r.DigestSources, "pods", "manifest", "registry")
	for img, repo := range r.Apps {
		path := k + ".apps." + img
		if strings.TrimSpace(img) == "" {
			p.add(k+".apps", "empty image repo key")
			continue
		}
		if strings.ContainsAny(img, ":@") {
			p.add(path, "key must be an image repo without tag or digest")
		}
		if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			p.add(path, "want owner/name, got %q", repo)
		}
	}
}

func validateEnvs(p *problems, k string, e EnvsConfig) {
	seen := map[string]bool{}
	for i, env := range e.Production {
		path := fmt.Sprintf("%s.production[%d]", k, i)
		if strings.TrimSpace(env) == "" {
			p.add(path, "empty env name")
			continue
		}
		checkNoSurroundingWhitespace(p, path, env)
		if seen[env] {
			p.add(path, "%q listed twice", env)
		}
		seen[env] = true
	}
	for src, dst := range e.Pairs {
		path := k + ".pairs." + src
		switch {
		case strings.TrimSpace(src) == "":
			p.add(k+".pairs", "empty source env")
		case strings.TrimSpace(dst) == "":
			p.add(path, "target env is empty")
		case src == dst:
			p.add(path, "an env cannot promote to itself")
		}
		checkNoSurroundingWhitespace(p, k+".pairs["+src+"] (key)", src)
		checkNoSurroundingWhitespace(p, path, dst)
	}
	for env, mode := range e.Approval {
		if strings.TrimSpace(env) == "" {
			p.add(k+".approval", "empty env name")
			continue
		}
		checkNoSurroundingWhitespace(p, k+".approval["+env+"] (key)", env)
		validateEnum(p, k+".approval."+env, mode, ApprovalComment, ApprovalAuto)
	}
}

func validateRegistry(p *problems, k string, r RegistryConfig) {
	if strings.TrimSpace(r.Prefix) == "" {
		p.add(k+".prefix", "is required")
	} else {
		checkNoSurroundingWhitespace(p, k+".prefix", r.Prefix)
	}
	validateList(p, k+".auth", r.Auth, "env", "keychain", "cluster", "op")
	if (r.Cluster.Namespace == "") != (r.Cluster.Secret == "") {
		p.add(k+".cluster", "needs both namespace and secret")
	}
	if r.Op != "" && !strings.HasPrefix(r.Op, "op://") {
		p.add(k+".op", "want an op://vault/item/field reference")
	}
}

// checkNoSurroundingWhitespace reports a value that yaml.v3 read literally (it never
// trims scalars) but that a later exact-match comparison — an env name against a wrapper's
// namespace, a map key against another env, an image repo prefix against a repo string —
// would never match because of it. Silence, not a loud error, is what surrounding
// whitespace would otherwise buy the operator.
func checkNoSurroundingWhitespace(p *problems, path, v string) {
	if strings.TrimSpace(v) != v {
		p.add(path, "%q has surrounding whitespace, which will never match", v)
	}
}

// validateEnum requires v to be one of allowed.
func validateEnum(p *problems, path, v string, allowed ...string) {
	for _, a := range allowed {
		if v == a {
			return
		}
	}
	p.add(path, "want one of %s, got %q", strings.Join(allowed, "|"), v)
}

// validateList requires a non-empty list of distinct values drawn from allowed.
func validateList(p *problems, path string, vs []string, allowed ...string) {
	if len(vs) == 0 {
		p.add(path, "must not be empty; omit it for the default %s", strings.Join(allowed, ", "))
		return
	}
	seen := map[string]bool{}
	for i, v := range vs {
		item := fmt.Sprintf("%s[%d]", path, i)
		if seen[v] {
			p.add(item, "%q listed twice", v)
		}
		seen[v] = true
		validateEnum(p, item, v, allowed...)
	}
}

// sortedErrs orders problems by message so map iteration cannot reorder a report.
func sortedErrs(errs []error) []error {
	sort.SliceStable(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errs
}

// Repo selects one repo: by name, by path as written, by expanded path (absolute or
// relative to the working directory), or the only entry when sel is empty and there is
// exactly one. No entries, or several with no selector, is an error listing the names.
// A selector that matches nothing wraps ErrUnknownRepo.
func (c *Config) Repo(sel string) (RepoConfig, error) {
	if sel == "" {
		switch len(c.Repos) {
		case 0:
			return RepoConfig{}, fmt.Errorf("%s: no repos configured", c.File)
		case 1:
			return c.Repos[0], nil
		default:
			return RepoConfig{}, fmt.Errorf("%s: %d repos configured; choose one with --repo <name|path>: %s", c.File, len(c.Repos), strings.Join(c.names(), ", "))
		}
	}
	abs, _ := filepath.Abs(sel)
	for _, r := range c.Repos {
		if r.Name == sel || r.Path == sel || (r.Dir != "" && (r.Dir == sel || r.Dir == abs)) {
			return r, nil
		}
	}
	return RepoConfig{}, fmt.Errorf("%w: %q is not in %s (repos: %s)", ErrUnknownRepo, sel, c.File, strings.Join(c.names(), ", "))
}

func (c *Config) names() []string {
	out := make([]string, 0, len(c.Repos))
	for _, r := range c.Repos {
		out = append(out, r.Name)
	}
	return out
}

// Redacted returns a copy with every secret-ish value replaced, for printing. Today that
// is registries[].op — a reference, not a secret, but the pattern is what future token
// fields follow — so `hoist config show` output can be pasted into an issue.
func (c *Config) Redacted() Config {
	out := *c
	out.Registries = append([]RegistryConfig(nil), c.Registries...)
	for i := range out.Registries {
		if out.Registries[i].Op != "" {
			out.Registries[i].Op = "<redacted>"
		}
	}
	return out
}

// Marshal renders the effective config as YAML: defaults filled in, paths as written.
func (c Config) Marshal() ([]byte, error) {
	return yaml.Marshal(&c)
}
