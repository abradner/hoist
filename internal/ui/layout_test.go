package ui

import "testing"

func TestDialogWidth(t *testing.T) {
	for w, want := range map[int]int{0: 20, 20: 20, 28: 20, 29: 21, 60: 52, 80: 72, 120: 72} {
		if got := DialogWidth(w); got != want {
			t.Errorf("DialogWidth(%d) = %d, want %d", w, got, want)
		}
	}
}

func TestBoolInt(t *testing.T) {
	if BoolInt(true) != 1 || BoolInt(false) != 0 {
		t.Fatal("BoolInt(true) must be 1 and BoolInt(false) 0")
	}
}

func TestShortSHA(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"abc":                          "abc",
		"0123456789ab":                 "0123456789ab",
		"0123456789abc":                "0123456789ab",
		"0123456789abcdef0123456789ab": "0123456789ab",
	} {
		if got := ShortSHA(in); got != want {
			t.Errorf("ShortSHA(%q) = %q, want %q", in, got, want)
		}
	}
}
