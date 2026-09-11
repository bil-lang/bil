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

// TestVetFailLineAfterAlt is TestVetFail's line-mapping counterpart: the
// violation here sits after a guarded `alt`, which bilc desugars into
// several lines of synthetic setup (bilGuard0 := ...; if !cond {...};
// select {...}) ahead of the real code — exactly the kind of line-count
// drift bilc's `//line` directives (see bilc.go's resync) exist to correct
// for. Confirms the reported position names the .bil file and its true
// original line, not the transpiled Go file bilc actually handed to vet.
func TestVetFailLineAfterAlt(t *testing.T) {
	const src = "testdata/vetfail/violation-after-alt.bil"
	const wantFirstPos = "violation-after-alt.bil:21: channel d is sent to in more than one par branch (branch 1, also branch 2 at "
	const wantSecondPos = "violation-after-alt.bil:24) — a channel may only be used for output in one component of a parallel"

	var stdout, stderr bytes.Buffer
	code := runCmd(src, false, &stdout, &stderr)
	if code == 0 {
		t.Fatal("runCmd(execute=false) returned 0, want non-zero")
	}
	got := stderr.String()
	if !strings.Contains(got, wantFirstPos) {
		t.Errorf("stderr = %q, want substring %q", got, wantFirstPos)
	}
	if !strings.Contains(got, wantSecondPos) {
		t.Errorf("stderr = %q, want substring %q", got, wantSecondPos)
	}
}

// TestTypeErrorLineAfterAlt is TestVetFailLineAfterAlt's counterpart for a
// plain Go type error (caught by vet.Check's own go/types pass, not one of
// Bil's usage rules) — a different message-formatting path in
// tools/vet/check.go, worth pinning separately since it goes through
// go/types' own error formatting rather than one of Check's fmt.Sprintf
// calls.
func TestTypeErrorLineAfterAlt(t *testing.T) {
	const src = "testdata/vetfail/type-error-after-alt.bil"
	const wantSubstr = "type-error-after-alt.bil:17: cannot use \"not a number\""

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
}

// TestPanicLineAfterAlt confirms line-mapping also holds for a runtime
// panic, not just vet's own static checks: this fixture passes every
// static check (no usage-rule violation, no type error) and only fails
// when actually run, via `go run`. The Go runtime's own panic/backtrace
// machinery honors the same `//line` directives at the compiler level, so
// this exercises a third, independent consumer of bilc's resync output
// (after tools/vet's AST-position reporting and go/types' own error
// formatting, both covered above).
func TestPanicLineAfterAlt(t *testing.T) {
	const src = "testdata/runfail/panic-after-alt.bil"
	const wantSubstr = "panic-after-alt.bil:18"

	var stdout, stderr bytes.Buffer
	code := runCmd(src, true, &stdout, &stderr)
	if code == 0 {
		t.Fatal("runCmd(execute=true) returned 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "panic: assignment to entry in nil map") {
		t.Errorf("stderr = %q, want a nil-map-assignment panic", stderr.String())
	}
	if !strings.Contains(stderr.String(), wantSubstr) {
		t.Errorf("stderr = %q, want substring %q", stderr.String(), wantSubstr)
	}
}
