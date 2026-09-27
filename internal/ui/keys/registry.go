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
// No screen reads this yet (T3-01's own scope note); the tests below hold it to the audit's
// rules mechanically rather than by review (AGENTS.md §10 meta-rule 5).
var table = []row{
	// matrix
	{ScrMatrix, Enter, "open the action menu for the cell; on the in-flight pane, open flight"},
	{ScrMatrix, Esc, "close the menu or overlay, else nothing"},
	{ScrMatrix, Quit, "quit (confirm if a promotion is driving)"},
	{ScrMatrix, Help, "help overlay"},
	{ScrMatrix, Refresh, "re-read the cluster and the repo"},
	{ScrMatrix, Open, "open the PR of the in-flight row (chooser if several)"},
	{ScrMatrix, Promote, "promote into the cursor column (source from the reverse pair, else asks)"},
	{ScrMatrix, Tag, "deploy a tag (tag picker for the cell)"},
	{ScrMatrix, Watch, "watch the cell's family"},
	{ScrMatrix, Restart, "restart the cell's family → restart screen"},
	{ScrMatrix, Abandon, "abandon the in-flight row (confirm)"},
	{ScrMatrix, Config, "config view"},
	{ScrMatrix, Log, "activity log"},
	{ScrMatrix, Up, "move row"},
	{ScrMatrix, Down, "move row"},
	{ScrMatrix, Left, "move column"},
	{ScrMatrix, Right, "move column"},
	{ScrMatrix, PgUp, "page"},
	{ScrMatrix, PgDn, "page"},
	{ScrMatrix, Tab, "table ⇄ in-flight pane"},
	{ScrMatrix, CtrlC, "quit now, printing what is in flight"},

	// action menu
	{ScrMenu, Enter, "run the highlighted item"},
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
	{ScrPlan, Refresh, "rebuild the plan at fresh origin"},
	{ScrPlan, Diff, "toggle yaml diff"},
	{ScrPlan, Direct, "toggle direct mode (confirm to turn on; never offered for production)"},
	{ScrPlan, Edit, "override the hovered repo's digest (input dialog)"},
	{ScrPlan, Space, "tick / untick repo"},
	{ScrPlan, Filter, "filter"},
	{ScrPlan, Tab, "repos ⇄ impact pane"},
	{ScrPlan, Log, "activity log"},
	{ScrPlan, Up, "move"},
	{ScrPlan, Down, "move"},
	{ScrPlan, PgUp, "page"},
	{ScrPlan, PgDn, "page"},
	{ScrPlan, CtrlC, "quit now"},

	// deploy confirm
	{ScrDeploy, Enter, "start deploy"},
	{ScrDeploy, Esc, "back to the picker"},
	{ScrDeploy, Help, "help overlay"},
	{ScrDeploy, Refresh, "rebuild the diff at fresh origin"},
	{ScrDeploy, Diff, "toggle yaml diff"},
	{ScrDeploy, Direct, "toggle direct mode (confirm to turn on; never offered for production)"},
	{ScrDeploy, Log, "activity log"},
	{ScrDeploy, Up, "scroll"},
	{ScrDeploy, Down, "scroll"},
	{ScrDeploy, PgUp, "page"},
	{ScrDeploy, PgDn, "page"},
	{ScrDeploy, CtrlC, "quit now"},

	// tag picker (list)
	{ScrTags, Enter, "review this tag → deploy confirm"},
	{ScrTags, Esc, "back"},
	{ScrTags, Help, "help overlay"},
	{ScrTags, Refresh, "reload tags"},
	{ScrTags, Open, "open the commit under the cursor on the forge"},
	{ScrTags, Filter, "filter"},
	{ScrTags, Tab, "list ⇄ commits"},
	{ScrTags, Right, "open the commit reader"},
	{ScrTags, Log, "activity log"},
	{ScrTags, Up, "move"},
	{ScrTags, Down, "move"},
	{ScrTags, Home, "top"},
	{ScrTags, End, "bottom"},
	{ScrTags, CtrlC, "quit now"},

	// tag picker's commit reader pane
	{ScrTagsReader, Left, "back to the list"},
	{ScrTagsReader, Help, "help overlay"},
	{ScrTagsReader, Open, "open the commit on the forge"},
	{ScrTagsReader, Tab, "commits ⇄ list"},
	{ScrTagsReader, Up, "switch commit"},
	{ScrTagsReader, Down, "switch commit"},
	{ScrTagsReader, PgUp, "scroll body"},
	{ScrTagsReader, PgDn, "scroll body"},
	{ScrTagsReader, CtrlC, "quit now"},

	// flight
	{ScrFlight, Esc, "back to the matrix, drive keeps running"},
	{ScrFlight, Help, "help overlay"},
	{ScrFlight, Refresh, "re-observe now"},
	{ScrFlight, Open, "open the PR"},
	{ScrFlight, Watch, "watch this promotion's family and target"},
	{ScrFlight, Abandon, "abandon (confirm)"},
	{ScrFlight, CINone, "treat \"no checks\" as green (confirm; only when offered)"},
	{ScrFlight, Log, "activity log (this promotion's lines first)"},
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
	{ScrWatch, CtrlC, "quit now"},

	// restart
	{ScrRestart, Enter, "restart (production: confirm dialog)"},
	{ScrRestart, Esc, "back (a rollout in progress continues, and the screen says so)"},
	{ScrRestart, Help, "help overlay"},
	{ScrRestart, Log, "activity log"},
	{ScrRestart, Up, "scroll"},
	{ScrRestart, Down, "scroll"},
	{ScrRestart, PgUp, "page"},
	{ScrRestart, PgDn, "page"},
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
