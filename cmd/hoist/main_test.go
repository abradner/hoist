package main

import (
	"os"
	"testing"
)

func TestRunVersion(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if got := run([]string{"-version"}, w, os.Stderr); got != 0 {
		t.Fatalf("exit %d, want 0", got)
	}
	_ = w.Close()
	buf := make([]byte, 64)
	n, _ := r.Read(buf)
	if got := string(buf[:n]); got != version+"\n" {
		t.Fatalf("stdout %q, want %q", got, version+"\n")
	}
}

func TestRunNoArgsIsUsageError(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	if got := run(nil, devnull, devnull); got != 2 {
		t.Fatalf("exit %d, want 2", got)
	}
}
