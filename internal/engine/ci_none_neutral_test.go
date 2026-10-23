package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abradner/hoist/pkg/forge"
)

// TestCINoneDetailsNameNoFaceGesture (UX-M2): the ci.none=prompt reason and the override's
// satisfied detail are shown by BOTH faces, so they may name neither the CLI's
// `--override-ci-none` nor the TUI's key; each face adds its own gesture beside the reason.
func TestCINoneDetailsNameNoFaceGesture(t *testing.T) {
	fx := newFixture(t)
	f := &forge.Fake{}
	s := driveToPR(t, fx, filepath.Join(t.TempDir(), "wt"), f)
	s.CINone = "prompt"
	s.CIGrace = time.Minute
	step := CIGreenStep{Forge: f, Now: fixedNow(s.PR.CreatedAt.Add(2 * time.Minute))}

	blocked, err := step.Observe(ctx(), s)
	if err != nil || !IsCINonePromptBlock(blocked.Blocked) {
		t.Fatalf("precondition: want the ci.none=prompt block, got %+v err=%v", blocked, err)
	}
	s.CINoneOverride = true
	overridden, err := step.Observe(ctx(), s)
	if err != nil || !overridden.Satisfied {
		t.Fatalf("precondition: want the override to satisfy, got %+v err=%v", overridden, err)
	}
	for name, text := range map[string]string{"blocked": blocked.Blocked, "overridden": overridden.Detail} {
		for _, face := range []string{"--override-ci-none", "hoist resume", "shift+c", "flight screen"} {
			if strings.Contains(text, face) {
				t.Errorf("%s text names a single face's gesture %q: %q", name, face, text)
			}
		}
	}
}
