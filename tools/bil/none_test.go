package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestNoneTargetRunsPlacedLocally checks `bil emu -target none` runs a
// placed-par program on this machine: every processor's clause body runs
// once, one goroutine per grid cell, with no emulator checkout involved.
func TestNoneTargetRunsPlacedLocally(t *testing.T) {
	const src = "testdata/none/placed.bil"
	var stdout, stderr bytes.Buffer
	in := mustRead(t, src)
	if code := runNoneTarget(src, in, 1, 2, &stdout, &stderr); code != 0 {
		t.Fatalf("runNoneTarget exited %d, stderr:\n%s", code, stderr.String())
	}
	got := stdout.String() + stderr.String()
	for _, want := range []string{"left", "right"} {
		if strings.Count(got, want+"\n") != 1 {
			t.Errorf("stdout = %q, want %q exactly once", got, want)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
