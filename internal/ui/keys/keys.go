package keys

import (
	"strings"
	"unicode"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Class is the semantic category a key belongs to, shared across every screen (audit rule 1):
// a key's class never changes screen to screen, only what it resolves to. TestOneClassPerKey
// asserts every key string in the registry maps to exactly one Class.
type Class uint8

const (
	// Primary is enter: the screen's one deliberate write or review action (rule 2).
	Primary Class = iota
	// Back is esc: always back, never a cancel of anything already running (rule 3).
	Back
	// Spatial is the arrow/page/home/end/tab family: movement, never a write.
	Spatial
	// Verb is a lowercase, unmodified action key shared in meaning across screens (rule 6):
	// r/F5/ctrl+r refresh, o open, d diff, w watch, l log, ? help, / filter, space toggle.
	Verb
	// Write is a key that starts or confirms a remote write, and for that reason always
	// carries the shift modifier (rule 5): shift+r, shift+x, shift+d, shift+c.
	Write
	// App is q and ctrl+c: whole-program control, not a screen concern.
	App
)

// Group is which help-overlay column a binding is listed under (v2·03 mockup: NAVIGATE, ACT,
// VIEW, APP).
type Group uint8

// The four help-overlay columns, in the order the v2·03 mockup lists them.
const (
	Navigate Group = iota
	Act
	View
	AppGroup
)

// Binding is one semantic key across the whole app: a Name (one meaning, matched against
// every screen it appears on by TestLetterOneMeaning), the Class and Group it belongs to, the
// bubbles match strings it responds to, and Show — the ONLY source a footer, the help overlay
// or a doc should render, so a write binding is never displayed as a bare capital letter
// (rule 5's own "never as the bare capital letter": a capital in a footer reads as "press this
// letter" and invites the caps-lock press a legacy terminal cannot tell from shift).
type Binding struct {
	Name  string
	Class Class
	Group Group
	// Keys are the bubbles/key match strings for a non-write binding. A write binding leaves
	// this nil and is matched by Matches below instead, which is stateless (train3-design.md,
	// "Matches for a write binding") rather than table-driven, since a write's whole point is
	// telling shift from a legacy uppercase byte, which no fixed string list can express.
	Keys []string
	// Show is the only display form: "r", "shift+r", "↑↓", "enter" — never derived from Keys,
	// so TestNoBareCapital can assert against exactly what a screen would render.
	Show string
	// write is the lower-case letter a Write-class binding matches on the legacy path
	// (Matches, below). Zero for every non-write binding.
	write rune
}

// write builds a Write-class Binding: shift+letter is what every surface shows (rule 5), and
// letter is what Matches actually tests, since a legacy terminal sends the same byte for
// shift+letter and caps-lock-then-letter and only Matches' own caps-lock check (point 3, from
// train3-design.md) can tell them apart when a protocol-capable terminal reports it.
func write(letter rune, name string) Binding {
	return Binding{
		Name:  name,
		Class: Write,
		Show:  "shift+" + string(letter),
		write: unicode.ToLower(letter),
	}
}

// Matches reports whether msg is this binding's key. For every non-write binding it defers to
// bubbles' key.Matches over Keys. For a write binding it runs the stateless four-rule test
// from train3-design.md's "Matches for a write binding" section:
//  1. the base letter must match, case-insensitively;
//  2. ctrl, alt or meta held rejects it outright — a write is a bare letter plus shift, never
//     a chord;
//  3. if the terminal reported ModCapsLock, ModShift must also be set — that combination is
//     only possible once flag 8 (ReportAllKeysAsEscapeCodes) was granted, and a caps-lock
//     letter with no shift is rejected rather than accidentally firing a write;
//  4. otherwise ModShift, an upper-case Text, or an upper-case Code all count — the last of
//     these is the rule that a legacy terminal's bare capital (no enhancement granted at all)
//     still counts as shift, since that is the only signal a legacy terminal ever sends.
func (b Binding) Matches(msg tea.KeyPressMsg) bool {
	if b.Class != Write {
		return key.Matches(msg, b.Bubbles())
	}
	k := msg.Key()
	if unicode.ToLower(k.Code) != b.write {
		return false
	}
	if k.Mod.Contains(tea.ModCtrl) || k.Mod.Contains(tea.ModAlt) || k.Mod.Contains(tea.ModMeta) {
		return false
	}
	if k.Mod.Contains(tea.ModCapsLock) {
		return k.Mod.Contains(tea.ModShift)
	}
	if k.Mod.Contains(tea.ModShift) {
		return true
	}
	if len(msg.Text) > 0 && unicode.IsUpper([]rune(msg.Text)[0]) {
		return true
	}
	return unicode.IsUpper(k.Code)
}

// Bubbles adapts a Binding for the components (viewport, table) that still take a bubbles
// key.Binding directly. A write binding has no fixed Keys list — WithKeys(upper) gives it the
// one legacy-path string a bare capital arrives as, which is the same string every terminal
// without flag 8 sends for both shift and caps lock (rule 5's "Modifier" note); Matches above
// is still what every screen in this package should call, since it alone can reject caps lock
// on a protocol-capable terminal.
func (b Binding) Bubbles() key.Binding {
	if b.Class == Write {
		return key.NewBinding(
			key.WithKeys(strings.ToUpper(string(b.write))),
			key.WithHelp(b.Show, b.Name),
		)
	}
	return key.NewBinding(key.WithKeys(b.Keys...), key.WithHelp(b.Show, b.Name))
}

// The shared bindings every screen's table (registry.go) is built from. Zero value means
// "unbound on this screen" — a Binding not listed in a Screen's On() result never fires there.
var (
	Enter  = Binding{Name: "primary", Class: Primary, Keys: []string{"enter"}, Show: "enter"}
	Esc    = Binding{Name: "back", Class: Back, Keys: []string{"esc"}, Show: "esc"}
	Up     = Binding{Name: "up", Class: Spatial, Keys: []string{"up", "k"}, Show: "↑"}
	Down   = Binding{Name: "down", Class: Spatial, Keys: []string{"down", "j"}, Show: "↓"}
	Left   = Binding{Name: "left", Class: Spatial, Keys: []string{"left"}, Show: "←"}
	Right  = Binding{Name: "right", Class: Spatial, Keys: []string{"right"}, Show: "→"}
	PgUp   = Binding{Name: "pgup", Class: Spatial, Keys: []string{"pgup"}, Show: "pgup"}
	PgDn   = Binding{Name: "pgdn", Class: Spatial, Keys: []string{"pgdown"}, Show: "pgdn"}
	Home   = Binding{Name: "home", Class: Spatial, Keys: []string{"home"}, Show: "home"}
	End    = Binding{Name: "end", Class: Spatial, Keys: []string{"end"}, Show: "end"}
	Tab    = Binding{Name: "tab", Class: Spatial, Keys: []string{"tab"}, Show: "tab"}
	Space  = Binding{Name: "space", Class: Verb, Keys: []string{"space"}, Show: "space"}
	Filter = Binding{Name: "filter", Class: Verb, Keys: []string{"/"}, Show: "/"}
	Help   = Binding{Name: "help", Class: Verb, Keys: []string{"?"}, Show: "?"}
	Quit   = Binding{Name: "quit", Class: App, Keys: []string{"q"}, Show: "q"}
	CtrlC  = Binding{Name: "ctrl-c", Class: App, Keys: []string{"ctrl+c"}, Show: "ctrl+c"}

	Refresh = Binding{Name: "refresh", Class: Verb, Keys: []string{"r", "f5", "ctrl+r"}, Show: "r"}
	Open    = Binding{Name: "open", Class: Verb, Keys: []string{"o"}, Show: "o"}
	Diff    = Binding{Name: "diff", Class: Verb, Keys: []string{"d"}, Show: "d"}
	Promote = Binding{Name: "promote", Class: Verb, Keys: []string{"p"}, Show: "p"}
	Tag     = Binding{Name: "tag", Class: Verb, Keys: []string{"t"}, Show: "t"}
	Watch   = Binding{Name: "watch", Class: Verb, Keys: []string{"w"}, Show: "w"}
	Log     = Binding{Name: "log", Class: Verb, Keys: []string{"l"}, Show: "l"}
	Config  = Binding{Name: "config", Class: Verb, Keys: []string{"c"}, Show: "c"}
	Edit    = Binding{Name: "edit", Class: Verb, Keys: []string{"e"}, Show: "e"}

	Restart = write('r', "restart")
	Abandon = write('x', "abandon")
	Direct  = write('d', "direct")
	CINone  = write('c', "ci-none")
)

// Screen names one screen's own row in the registry. ScrTagsReader is the tag picker's commit
// pane, which unbinds and rebinds a few of the list's own keys (→/← swap meaning) rather than
// being a screen the app ever pushes on its own stack.
//
// Deviation from train3-design.md's literal spelling: the design's Binding-var block and its
// Screen-const comment both name Watch, Restart and Config, which cannot coexist as Go
// identifiers at package scope. Every Screen constant here carries a "Scr" prefix instead, so
// keys.Watch/keys.Restart/keys.Config stay the Binding vars the design's four-write-binding
// note describes, and keys.ScrWatch/keys.ScrRestart/keys.ScrConfig name the screens.
type Screen string

// The registry's screens, in the audit's own "Screen × key" column order.
const (
	ScrMatrix     Screen = "matrix"
	ScrMenu       Screen = "menu"
	ScrPlan       Screen = "plan"
	ScrDeploy     Screen = "deploy"
	ScrTags       Screen = "tags"
	ScrTagsReader Screen = "tags-reader"
	ScrFlight     Screen = "flight"
	ScrWatch      Screen = "watch"
	ScrRestart    Screen = "restart"
	ScrConfig     Screen = "config"
	ScrActivity   Screen = "activity"
)

// Entry pairs a Binding with the one-line description On(screen) shows it with — the same
// description text the help overlay and docs/guide.md draw from, so the wording only exists
// once.
type Entry struct {
	Binding
	Desc string
}
