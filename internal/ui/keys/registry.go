package keys

// row is one cell of the approved screen × key table (the audit's "Screen × key" section,
// docs/audit/2026-09-ux-arch-audit.md). table is the whole table as data: every other function
// in this file is a view over it, so a new screen or a retired key changes one place.
type row struct {
	Screen Screen
	B      Binding
	Desc   string
}

// table is the whole approved screen × key matrix, transcribed from the audit's own table.
// Every screen in internal/app reads it through On/Has below (T3-01 through T3-10 finished the
// migration); the tests in this file and each screen package's own TestRegistryKeysAreHonoured
// (internal/app/matrix, plan, deploy, tags, flight, watch, restart, config, activity) hold it to
// the audit's rules mechanically rather than by review (AGENTS.md §10 meta-rule 5).
var table = []row{
	// matrix
	{ScrMatrix, Enter, "open menu / flight"},
	{ScrMatrix, Esc, "close menu/overlay"},
	{ScrMatrix, Quit, "quit (asks if busy)"},
	{ScrMatrix, Help, "help overlay"},
	{ScrMatrix, Refresh, "re-read cluster/repo"},
	{ScrMatrix, Open, "open PR (chooser)"},
	{ScrMatrix, Promote, "promote into column"},
	{ScrMatrix, Tag, "deploy a tag"},
	{ScrMatrix, Watch, "watch family"},
	{ScrMatrix, Restart, "restart family"},
	{ScrMatrix, Abandon, "abandon row"},
	{ScrMatrix, Config, "config view"},
	{ScrMatrix, Log, "activity log"},
	{ScrMatrix, Up, "move row"},
	{ScrMatrix, Down, "move row"},
	{ScrMatrix, Left, "move column"},
	{ScrMatrix, Right, "move column"},
	{ScrMatrix, PgUp, "page"},
	{ScrMatrix, PgDn, "page"},
	{ScrMatrix, Home, "top"},
	{ScrMatrix, End, "bottom"},
	{ScrMatrix, Tab, "table/flight pane"},
	{ScrMatrix, CtrlC, "quit, print flight"},

	// action menu
	{ScrMenu, Enter, "run highlighted item"},
	{ScrMenu, Esc, "close"},
	{ScrMenu, Help, "help overlay"},
	{ScrMenu, Promote, "promote into"},
	{ScrMenu, Tag, "deploy a tag"},
	{ScrMenu, Watch, "watch"},
	{ScrMenu, Restart, "restart"},
	{ScrMenu, Up, "move"},
	{ScrMenu, Down, "move"},
	{ScrMenu, CtrlC, "quit now"},

	// plan confirm
	{ScrPlan, Enter, "start promotion"},
	{ScrPlan, Esc, "back"},
	{ScrPlan, Help, "help overlay"},
	{ScrPlan, Refresh, "rebuild from origin"},
	{ScrPlan, Diff, "toggle yaml diff"},
	{ScrPlan, Direct, "direct/no prod"},
	{ScrPlan, Edit, "override digest"},
	{ScrPlan, Space, "tick / untick repo"},
	{ScrPlan, Filter, "filter"},
	{ScrPlan, Tab, "repos ⇄ impact pane"},
	{ScrPlan, Log, "activity log"},
	{ScrPlan, Up, "move"},
	{ScrPlan, Down, "move"},
	{ScrPlan, PgUp, "page"},
	{ScrPlan, PgDn, "page"},
	{ScrPlan, Home, "top"},
	{ScrPlan, End, "bottom"},
	{ScrPlan, CtrlC, "quit now"},

	// deploy confirm
	{ScrDeploy, Enter, "start deploy"},
	{ScrDeploy, Esc, "back to the picker"},
	{ScrDeploy, Help, "help overlay"},
	{ScrDeploy, Refresh, "rebuild from origin"},
	{ScrDeploy, Diff, "toggle yaml diff"},
	{ScrDeploy, Direct, "direct/no prod"},
	{ScrDeploy, Log, "activity log"},
	{ScrDeploy, Up, "scroll"},
	{ScrDeploy, Down, "scroll"},
	{ScrDeploy, PgUp, "page"},
	{ScrDeploy, PgDn, "page"},
	{ScrDeploy, Home, "top"},
	{ScrDeploy, End, "bottom"},
	{ScrDeploy, CtrlC, "quit now"},

	// tag picker (list). Open ("o") was listed and shown in help but
	// implemented nowhere in this package — no forge commit URL is plumbed into this screen at
	// all (unlike flight's OpenPRMsg, which the root already threads through) — so it is
	// delisted here rather than left as a dead row; wiring a real "open commit on forge" gesture
	// is a real feature (a URL builder, a message, a root handler) and belongs in its own
	// change. Home/End are implemented (moveCursor's own list) and now listed alongside them.
	{ScrTags, Enter, "review tag → confirm"},
	{ScrTags, Esc, "back"},
	{ScrTags, Help, "help overlay"},
	{ScrTags, Refresh, "reload tags"},
	{ScrTags, Filter, "filter"},
	{ScrTags, Tab, "list ⇄ commits"},
	{ScrTags, Right, "open commit reader"},
	{ScrTags, Log, "activity log"},
	{ScrTags, Up, "move"},
	{ScrTags, Down, "move"},
	{ScrTags, Home, "top"},
	{ScrTags, End, "bottom"},
	{ScrTags, CtrlC, "quit now"},

	// tag picker's commit reader pane. Open delisted for the same reason as ScrTags'
	// own row above; Tab was listed ("commits ⇄ list") but updateReading's own switch has no
	// case for it — reading a commit has no second pane to tab to — so it is delisted too.
	// Esc, Enter, Home and End ARE handled (updateReading: Back/Left, Review, Home, End) but
	// were missing from this row entirely; listed now.
	{ScrTagsReader, Esc, "back to the list"},
	{ScrTagsReader, Left, "back to the list"},
	{ScrTagsReader, Enter, "review tag → confirm"},
	{ScrTagsReader, Help, "help overlay"},
	{ScrTagsReader, Up, "switch commit"},
	{ScrTagsReader, Down, "switch commit"},
	{ScrTagsReader, PgUp, "scroll body"},
	{ScrTagsReader, PgDn, "scroll body"},
	{ScrTagsReader, Home, "top"},
	{ScrTagsReader, End, "bottom"},
	{ScrTagsReader, CtrlC, "quit now"},

	// flight
	{ScrFlight, Esc, "back (keeps running)"},
	{ScrFlight, Help, "help overlay"},
	{ScrFlight, Refresh, "re-observe now"},
	{ScrFlight, Open, "open the PR"},
	{ScrFlight, Watch, "watch family/target"},
	{ScrFlight, Abandon, "abandon"},
	{ScrFlight, CINone, "no CI = green"},
	{ScrFlight, Log, "log (mine first)"},
	{ScrFlight, Up, "scroll log"},
	{ScrFlight, Down, "scroll log"},
	{ScrFlight, PgUp, "page"},
	{ScrFlight, PgDn, "page"},
	{ScrFlight, Home, "top"},
	{ScrFlight, End, "bottom"},
	{ScrFlight, CtrlC, "quit now"},

	// watch
	{ScrWatch, Esc, "back"},
	{ScrWatch, Help, "help overlay"},
	{ScrWatch, Refresh, "refresh"},
	{ScrWatch, Log, "activity log"},
	{ScrWatch, Up, "scroll"},
	{ScrWatch, Down, "scroll"},
	{ScrWatch, PgUp, "page"},
	{ScrWatch, PgDn, "page"},
	{ScrWatch, Home, "top"},
	{ScrWatch, End, "bottom"},
	{ScrWatch, CtrlC, "quit now"},

	// restart
	{ScrRestart, Enter, "restart (confirms)"},
	{ScrRestart, Esc, "back (keeps running)"},
	{ScrRestart, Help, "help overlay"},
	// the design's own re-read row was missing entirely — implemented now
	// (Model.reread) rather than delisted, since Funcs.Read was already there to reuse.
	{ScrRestart, Refresh, "re-read the cluster"},
	{ScrRestart, Log, "activity log"},
	{ScrRestart, Up, "scroll"},
	{ScrRestart, Down, "scroll"},
	{ScrRestart, PgUp, "page"},
	{ScrRestart, PgDn, "page"},
	{ScrRestart, Home, "top"},
	{ScrRestart, End, "bottom"},
	{ScrRestart, CtrlC, "quit now"},

	// config: a static read, no re-read verb (AGENTS.md §4.8)
	{ScrConfig, Esc, "back"},
	{ScrConfig, Help, "help overlay"},
	{ScrConfig, Log, "activity log"},
	{ScrConfig, Up, "scroll"},
	{ScrConfig, Down, "scroll"},
	{ScrConfig, PgUp, "page"},
	{ScrConfig, PgDn, "page"},
	{ScrConfig, Home, "top"},
	{ScrConfig, End, "bottom"},
	{ScrConfig, CtrlC, "quit now"},

	// activity log
	{ScrActivity, Esc, "back"},
	{ScrActivity, Help, "help overlay"},
	{ScrActivity, Up, "scroll"},
	{ScrActivity, Down, "scroll"},
	{ScrActivity, PgUp, "page"},
	{ScrActivity, PgDn, "page"},
	// the design's own home/end row was missing on every viewport-backed
	// screen; implemented here (and on plan/deploy/watch/restart/matrix) rather than delisted,
	// since every one of them already has a list or viewport to jump.
	{ScrActivity, Home, "top"},
	{ScrActivity, End, "bottom"},
	{ScrActivity, CtrlC, "quit now"},
}

// On returns every entry bound on screen s, in table order.
func On(s Screen) []Entry {
	var out []Entry
	for _, r := range table {
		if r.Screen == s {
			out = append(out, Entry{Binding: r.B, Desc: r.Desc})
		}
	}
	return out
}

// Has reports whether b is bound on screen s — compared by Name, a Binding's one stable
// identity, since two Binding values built the same way are not otherwise comparable in a way
// that survives being copied into a row.
func Has(s Screen, b Binding) bool {
	for _, r := range table {
		if r.Screen == s && r.B.Name == b.Name {
			return true
		}
	}
	return false
}

// Screens lists every screen with at least one row, in first-appearance order — used by tests
// that check a property holds everywhere rather than needing their own screen list kept in
// sync with table by hand.
func Screens() []Screen {
	var out []Screen
	seen := map[Screen]bool{}
	for _, r := range table {
		if !seen[r.Screen] {
			seen[r.Screen] = true
			out = append(out, r.Screen)
		}
	}
	return out
}
