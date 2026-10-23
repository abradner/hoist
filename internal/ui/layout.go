package ui

// DialogWidth is the width of a modal dialog's body on a terminal w columns wide: 8 columns
// narrower than the terminal (the dimmed parent shows around it), never wider than 72 and never
// narrower than 20. Every screen that raises a huh field in a ui.Dialog sizes it with this, so
// the dialogs agree on a width rather than each screen repeating the clamp.
func DialogWidth(w int) int { return max(min(w-8, 72), 20) }

// BoolInt is 1 for true and 0 for false, for row arithmetic like `rows -= h * ui.BoolInt(cond)`.
func BoolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ShortSHA shortens a commit sha for display to its first 12 characters; a shorter string is
// returned unchanged. It is not for digests or revisions that want a different length or a
// placeholder for empty (watch's shortRev, deploy's 7-character log sha) — those keep their own
// functions so their output never changes.
func ShortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
