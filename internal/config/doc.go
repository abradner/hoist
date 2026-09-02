// Package config loads hoist's config file: $XDG_CONFIG_HOME/hoist/config.yaml, or
// ~/.config/hoist/config.yaml when XDG_CONFIG_HOME is unset — the same rule on every
// platform, so the docs stay one sentence — overridable with --config <path>.
//
// Shape (no convention was stated for configuration before this package; AGENTS.md §8,
// "building structure where no convention is stated is a decision" — this is the proposal):
//
//   - One typed struct per YAML mapping, decoded with yaml.v3 and KnownFields(true): a
//     misspelt key is an error naming the line, never a silently ignored setting.
//   - Defaults are applied in exactly one step, Normalize, after decoding and before
//     Validate; no consumer reads a zero value and guesses. Required values have no
//     default (AGENTS.md §8, "Configuration"): a missing one is a Validate error.
//   - Every validation error carries the file and the YAML path of the offending value
//     (config.yaml: repos[0].envs.pairs.app-staging: …), and Validate reports all of them
//     at once rather than the first.
//   - Load reads one file and nothing else: it never runs a program, reads the network,
//     or touches the repo the file describes. Cross-checks that need the world — whether
//     an env named in envs.production exists, whether a kube context is reachable —
//     belong to the consumer that has already observed the world (AGENTS.md §4.1).
//   - A missing file is not an error (Found is false and the config is the defaults, so
//     the CLI can run on flags alone); a file that exists and does not parse is always an
//     error. Ignoring a broken file would turn a typo into a silent change of behaviour.
//   - Values are kept as written: Path holds the user's ~/…; the expanded form is the
//     derived Dir field. `hoist config show` therefore prints what the user wrote plus
//     the defaults, and never a resolved home directory.
package config
