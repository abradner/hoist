package engine

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// legacyNoisyHistory is what a state written before History recorded changes only looks like:
// every walk re-appended every step's observation. Four real acts, then 200 polls of
// "4 satisfied steps + a CI wait", the wait changing once (2/4 -> 3/4) halfway: 4 + 200*5 = 1004
// entries holding 10 facts. Entry i is stamped i seconds after base, so a first occurrence's time
// is checkable.
func legacyNoisyHistory(base time.Time) []HistoryEntry {
	var h []HistoryEntry
	add := func(step StepName, detail string) {
		h = append(h, HistoryEntry{Step: step, At: base.Add(time.Duration(len(h)) * time.Second), Detail: detail})
	}
	steps := []StepName{StepBranched, StepCommitted, StepPushed, StepPROpened}
	for _, s := range steps {
		add(s, "acted")
	}
	for i := 0; i < 200; i++ {
		for _, s := range steps {
			add(s, "already satisfied: ok")
		}
		wait := "waiting: CI: 2/4 checks complete"
		if i >= 100 {
			wait = "waiting: CI: 3/4 checks complete"
		}
		add(StepCIGreen, wait)
	}
	return h
}

func TestCompactHistoryCollapsesLegacyNoise(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	noisy := legacyNoisyHistory(base)
	if len(noisy) != 1004 {
		t.Fatalf("fixture has %d entries, want 1004", len(noisy))
	}
	got := CompactHistory(noisy)
	var details []string
	for _, e := range got {
		details = append(details, fmt.Sprintf("%s|%s", e.Step, e.Detail))
	}
	want := []string{
		"branched|acted", "committed|acted", "pushed|acted", "pr-opened|acted",
		"branched|already satisfied: ok", "committed|already satisfied: ok", "pushed|already satisfied: ok", "pr-opened|already satisfied: ok",
		"ci-green|waiting: CI: 2/4 checks complete", "ci-green|waiting: CI: 3/4 checks complete",
	}
	if !reflect.DeepEqual(details, want) {
		t.Fatalf("compacted to %d entries:\n%v\nwant:\n%v", len(got), details, want)
	}
	// First occurrences keep their own times: the first "already satisfied" is entry 4, the
	// first 2/4 wait entry 8, the first 3/4 wait the fifth entry of poll 100.
	for i, idx := range map[int]int{4: 4, 8: 8, 9: 4 + 100*5 + 4} {
		if !got[i].At.Equal(noisy[idx].At) {
			t.Errorf("entry %d lost its first-occurrence time: %v, want %v", i, got[i].At, noisy[idx].At)
		}
	}
	if len(noisy) != 1004 {
		t.Fatal("CompactHistory modified its input")
	}
}

func TestCompactHistoryIsIdempotentAndNoOpOnCleanHistory(t *testing.T) {
	once := CompactHistory(legacyNoisyHistory(time.Now()))
	twice := CompactHistory(once)
	if !reflect.DeepEqual(once, twice) {
		t.Fatalf("compaction is not idempotent:\n%v\n%v", once, twice)
	}
	// A History the current engine wrote — including a stop that returns after another step's
	// entry and a repeated acted — has nothing to drop.
	clean := []HistoryEntry{
		{Step: StepCIGreen, Detail: "already satisfied: green"},
		{Step: StepApproved, Detail: "waiting: no approval yet"},
		{Step: StepCIGreen, Detail: "waiting: CI: 3/4"},
		{Step: StepCIGreen, Detail: "already satisfied: green again"},
		{Step: StepApproved, Detail: "waiting: no approval yet"},
		{Step: StepArgoRefreshed, Detail: "acted"},
		{Step: StepArgoRefreshed, Detail: "acted"},
	}
	if got := CompactHistory(clean); !reflect.DeepEqual(got, clean) {
		t.Fatalf("clean History changed:\n%v", got)
	}
	if got := CompactHistory(nil); got != nil {
		t.Fatalf("nil History must stay nil, got %v", got)
	}
}

// TestLoadStateCompactsLegacyHistoryInMemoryOnly: the file keeps its 1004 entries until the next
// ordinary save; what LoadState hands back is already compact.
func TestLoadStateCompactsLegacyHistoryInMemoryOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	legacy := &PromotionState{ID: "abcd1234", History: legacyNoisyHistory(time.Now())}
	if err := SaveState(path, legacy); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 10 {
		t.Fatalf("LoadState returned %d entries, want 10", len(got.History))
	}
	if err := SaveState(path, got); err != nil {
		t.Fatal(err)
	}
	again, err := LoadState(path)
	if err != nil || len(again.History) != 10 {
		t.Fatalf("after the next save: %d entries, err %v", len(again.History), err)
	}
}
