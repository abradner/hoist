package gitops

import (
	"bytes"
	"fmt"
	"strings"
)

// diffContext is the number of unchanged lines shown around each change.
const diffContext = 3

// UnifiedDiff renders before → after for one file as a unified diff with three lines of
// context. It is written for the shape hoist produces — line-for-line replacements with an
// unchanged line count — and needs no LCS. Should the line counts differ (Verify would have
// rejected that), it falls back to one whole-file hunk so the output is still truthful.
// Returns "" when the inputs are identical.
func UnifiedDiff(path string, before, after []byte) string {
	if bytes.Equal(before, after) {
		return ""
	}
	bl, bnl := splitForDiff(before)
	al, anl := splitForDiff(after)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	if len(bl) != len(al) {
		fmt.Fprintf(&sb, "@@ -1,%d +1,%d @@\n", len(bl), len(al))
		for i, l := range bl {
			writeDiffLine(&sb, '-', l, i == len(bl)-1 && !bnl)
		}
		for i, l := range al {
			writeDiffLine(&sb, '+', l, i == len(al)-1 && !anl)
		}
		return sb.String()
	}
	var changed []int
	for i := range bl {
		if !bytes.Equal(bl[i], al[i]) {
			changed = append(changed, i)
		}
	}
	last := len(bl) - 1
	for i := 0; i < len(changed); {
		start, end := changed[i], changed[i]
		j := i + 1
		for j < len(changed) && changed[j]-end <= 2*diffContext+1 {
			end = changed[j]
			j++
		}
		hs := max(0, start-diffContext)
		he := min(len(bl), end+diffContext+1)
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", hs+1, he-hs, hs+1, he-hs)
		for k := hs; k < he; {
			if bytes.Equal(bl[k], al[k]) {
				writeDiffLine(&sb, ' ', bl[k], k == last && !bnl)
				k++
				continue
			}
			r := k
			for r < he && !bytes.Equal(bl[r], al[r]) {
				r++
			}
			for m := k; m < r; m++ {
				writeDiffLine(&sb, '-', bl[m], m == last && !bnl)
			}
			for m := k; m < r; m++ {
				writeDiffLine(&sb, '+', al[m], m == last && !anl)
			}
			k = r
		}
		i = j
	}
	return sb.String()
}

// splitForDiff splits into lines without their newline and reports whether the input ended
// with one.
func splitForDiff(b []byte) (lines [][]byte, trailingNewline bool) {
	if len(b) == 0 {
		return nil, true
	}
	lines = bytes.Split(b, []byte{'\n'})
	if len(lines[len(lines)-1]) == 0 {
		return lines[:len(lines)-1], true
	}
	return lines, false
}

func writeDiffLine(sb *strings.Builder, prefix byte, line []byte, noNewline bool) {
	sb.WriteByte(prefix)
	sb.Write(line)
	sb.WriteByte('\n')
	if noNewline {
		sb.WriteString("\\ No newline at end of file\n")
	}
}
