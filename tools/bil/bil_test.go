package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunOK runs every testdata/ok/*.bil through runCmd with execute=true
// and checks combined stdout+stderr against the matching *.golden file.
func TestRunOK(t *testing.T) {
	matches, err := filepath.Glob("testdata/ok/*.bil")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no testdata/ok/*.bil fixtures found")
	}
	for _, src := range matches {
		src := src
		t.Run(filepath.Base(src), func(t *testing.T) {
			golden, err := os.ReadFile(strings.TrimSuffix(src, ".bil") + ".golden")
			if err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			if code := runCmd(src, true, &out, &out); code != 0 {
				t.Fatalf("runCmd exited %d, output:\n%s", code, out.String())
			}
			if out.String() != string(golden) {
				t.Errorf("output mismatch\ngot:\n%s\nwant:\n%s", out.String(), golden)
			}
		})
	}
}

// TestVetFail checks that a fixture which transpiles cleanly but violates a
// par-branch usage rule (two branches sending on the same channel) is
// rejected by `bil run` — with the program never executed — and by
// `bil vet` on its own.
func TestVetFail(t *testing.T) {
	const src = "testdata/vetfail/double-send.bil"
	const wantSubstr = "is sent to in more than one par branch"

	t.Run("run", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runCmd(src, true, &stdout, &stderr)
		if code == 0 {
			t.Fatal("runCmd(execute=true) returned 0, want non-zero")
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout not empty, program should not have run: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), wantSubstr) {
			t.Errorf("stderr = %q, want substring %q", stderr.String(), wantSubstr)
		}
	})

	t.Run("vet", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runCmd(src, false, &stdout, &stderr)
		if code == 0 {
			t.Fatal("runCmd(execute=false) returned 0, want non-zero")
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout not empty: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), wantSubstr) {
			t.Errorf("stderr = %q, want substring %q", stderr.String(), wantSubstr)
		}
	})
}
