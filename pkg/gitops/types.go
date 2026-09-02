package gitops

import (
	"time"

	"gopkg.in/yaml.v3"

	"github.com/abradner/hoist/pkg/image"
)

// Occurrence is one image: scalar inside a container item of one manifest document.
type Occurrence struct {
	// File is the manifest path relative to the repo root, slash-separated.
	File string
	// Doc is the 0-based index of the document within File.
	Doc int
	// Line is 1-based and file-absolute: yaml.v3 does not reset it per document.
	Line int
	// Col is yaml.v3's Column for the scalar node: 1-based, counted in characters, and
	// pointing at the first character of the scalar *token*. For a quoted scalar that is the
	// opening quote, so the value itself starts one column later (verified against yaml.v3
	// v3.0.1 — the docs are silent on it).
	//
	// This deviates knowingly from the M1 brief, which describes the column as "after any
	// quote": Col records what yaml.v3 emits, unchanged, rather than a derived value that
	// would have to be kept in step with the parser. Apply and Verify compensate — both step
	// past the opening quote for DoubleQuotedStyle/SingleQuotedStyle before matching Raw — so
	// the brief's intent (the edit replaces exactly the value, never the quotes) holds.
	// Callers should treat Col as opaque.
	Col int
	// Style is the scalar's yaml.v3 style: 0 plain, DoubleQuotedStyle, SingleQuotedStyle, or a
	// block style (LiteralStyle/FoldedStyle), which is recorded but refused by Apply.
	Style yaml.Style
	// Kind and Name identify the enclosing document (kind, metadata.name).
	Kind, Name string
	// Container is the item's name field, "" when absent.
	Container string
	// Path is the dotted YAML path of the scalar, e.g. spec.template.spec.containers[0].image.
	Path string
	// Raw is the scalar text exactly as decoded (node.Value). Apply searches for this text at
	// Line/Col; it exists as a separate field because Ref.String() normalises (drops a
	// docker-pullable:// prefix, for one) and so cannot serve as the search key.
	Raw string
	// Ref is the parsed reference.
	Ref image.Ref
}

// Family is one deployable unit inside an env, backed by exactly one Argo Application.
type Family struct {
	Name        string // base name of Dir
	Dir         string // spec.source.path, relative to the repo root
	App         string // the Application's metadata.name
	Occurrences []Occurrence
}

// Env is one environment: the Argo destination namespace of its Applications.
type Env struct {
	Name string
	// Dir is the directory holding this env's family directories when every family shares
	// one parent, else "". It is informational; families carry their own Dir.
	Dir      string
	Families map[string]*Family // keyed by Family.Name
}

// ArgoApp is one kind: Application wrapper as read from the apps root.
type ArgoApp struct {
	Name       string
	SourcePath string // spec.source.path
	Namespace  string // spec.destination.namespace
	// File is the wrapper file that declared it, relative to the repo root — so a report can
	// point at the wrapper rather than guessing from the family name.
	File string
}

// Repo is the discovered shape of one GitOps checkout.
type Repo struct {
	Root     string
	AppsRoot string // relative to Root
	Envs     map[string]*Env
	Apps     []ArgoApp
	// Unmanaged lists directories (relative to Root) under the apps root, beside a managed
	// family, or nested inside one, that hold YAML manifests but are the source path of no
	// Application. They are never scanned for promotion: a family is read one level deep, so
	// a nested subdirectory's manifests are reported here rather than silently ignored.
	Unmanaged []string
}

// Edit is one planned scalar replacement.
type Edit struct {
	Occurrence
	New image.Ref
}

// NoOp reports whether the target already carries exactly the planned reference. The edit
// is still listed so that every occurrence is accounted for; Apply leaves the bytes alone.
func (e Edit) NoOp() bool { return e.Ref.String() == e.New.String() }

// Warning is something the operator should see but that does not block the plan
// (AGENTS.md principle 5). Code is one of the Warn* constants.
type Warning struct {
	Code        string
	Message     string
	Occurrences []Occurrence
}

// Plan is the result of BuildPlan for one env pair.
type Plan struct {
	SourceEnv string
	TargetEnv string
	Edits     []Edit
	// Untouched lists the distinct target-env references no Edit touches: third-party images
	// (repo outside the promotable prefixes) and repos absent from the source env.
	Untouched   []image.Ref
	Warnings    []Warning
	GeneratedAt time.Time
}
