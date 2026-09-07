package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOK runs every testdata/ok/*.bil through Transform, `go run`s the
// result, and checks stdout against the matching *.golden file.
func TestOK(t *testing.T) {
	files, err := filepath.Glob("testdata/ok/*.bil")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testdata/ok/*.bil files found")
	}
	for _, bilFile := range files {
		bilFile := bilFile
		name := strings.TrimSuffix(filepath.Base(bilFile), ".bil")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(bilFile)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Transform(src)
			if err != nil {
				t.Fatalf("Transform: %v", err)
			}

			goldenPath := strings.TrimSuffix(bilFile, ".bil") + ".golden"
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("missing golden file %s: %v", goldenPath, err)
			}

			goFile := filepath.Join(t.TempDir(), "main.go")
			if err := os.WriteFile(goFile, out, 0o644); err != nil {
				t.Fatal(err)
			}

			// -race: static checks below can't see every par-branch
			// shape (bare `{...}`/`seq{...}` branches closing over a
			// free variable slip past all of them — see the Snagging
			// list), so every ok fixture also gets a dynamic pass. Not
			// a substitute for the static checks — only catches a race
			// that actually fires on this run — but it's caught real
			// bugs no static check here does yet.
			cmd := exec.Command("go", "run", "-race", goFile)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stdout
			if err := cmd.Run(); err != nil {
				t.Fatalf("go run -race %s: %v\noutput:\n%s", goFile, err, stdout.String())
			}

			if got := stdout.String(); got != string(want) {
				t.Errorf("output mismatch\n--- got ---\n%s--- want ---\n%s", got, string(want))
			}
		})
	}
}

// TestErr runs every testdata/err/*.bil through Transform and checks that it
// fails with an error containing the matching *.err file's text.
func TestErr(t *testing.T) {
	files, err := filepath.Glob("testdata/err/*.bil")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testdata/err/*.bil files found")
	}
	for _, bilFile := range files {
		bilFile := bilFile
		name := strings.TrimSuffix(filepath.Base(bilFile), ".bil")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(bilFile)
			if err != nil {
				t.Fatal(err)
			}

			wantPath := strings.TrimSuffix(bilFile, ".bil") + ".err"
			want, err := os.ReadFile(wantPath)
			if err != nil {
				t.Fatalf("missing expected-error file %s: %v", wantPath, err)
			}
			wantStr := strings.TrimSpace(string(want))

			_, err = Transform(src)
			if err == nil {
				t.Fatalf("Transform succeeded, want error containing %q", wantStr)
			}
			if !strings.Contains(err.Error(), wantStr) {
				t.Errorf("error mismatch\ngot:  %s\nwant substring: %s", err.Error(), wantStr)
			}
		})
	}
}

// TestBareBlockRaceCaughtDynamically documents a known, currently-open gap
// in bilc itself: bilc has no static check left for general variable usage
// or point-to-point channel usage at all (both moved entirely to
// tools/static_check's variables.go/channels.go, which — unlike the bilc
// checks they replaced — aren't restricted to call-shaped branches, so a
// bare `{...}` or `seq{...}` branch closing over and mutating a variable
// directly (both legal shapes, sanctioned by Example 1's design notes) is
// no longer even a residual gap *there* — static_check does catch this
// example, confirmed directly). This test proves TestOK's `-race` pass
// does, as bilc's own dynamic backstop for bilc's own now-total absence of
// a static check here — not a fix, and not sound (a race the detector
// doesn't happen to hit on a given run still slips through), but real
// coverage for bilc's real hole, one static_check itself has already
// closed for anyone running the combined tools/bilc-run pipeline rather
// than bare `bilc` — bilc's own checks have been progressively removed in
// static_check's favor.
func TestBareBlockRaceCaughtDynamically(t *testing.T) {
	src := []byte(`package main

func main() {
	x := 0
	par {
		{
			for i := 0; i < 100000; i++ {
				x = i
			}
		}
		{
			for i := 0; i < 100000; i++ {
				_ = x
			}
		}
	}
}
`)
	out, err := Transform(src)
	if err != nil {
		t.Fatalf("Transform: %v (if this now fails, a static check has closed the gap — replace this test with an err/ fixture instead)", err)
	}

	goFile := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(goFile, out, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "-race", goFile)
	out2, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out2), "WARNING: DATA RACE") {
		t.Errorf("expected go run -race to catch the bare-block closure-capture race, got:\n%s", out2)
	}
}

// TestStopRewrite checks that a bare `stop` statement rewrites to a
// non-busy, effectively-permanent block (time.Sleep with the max
// time.Duration — NOT select{}, which the Go runtime treats as provably
// permanent and crashes on with a deadlock panic the moment nothing else
// is alive, contradicting STOP's actual meaning). It can't use the TestOK
// ok/*.bil shape: whatever calls `stop` never returns, so there's no
// well-defined stdout to diff against — building (not running) is the
// right level of check here.
func TestStopRewrite(t *testing.T) {
	src := []byte(`package main

proc idle() {
	stop
}

func main() {
}
`)
	out, err := Transform(src)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	if strings.Contains(string(out), "select {}") {
		t.Errorf("`stop` must not rewrite to select{} (crashes as the last live goroutine), got:\n%s", out)
	}
	if !strings.Contains(string(out), "time.Sleep(1<<63 - 1)") {
		t.Errorf("expected `stop` to rewrite to `time.Sleep(1<<63 - 1)`, got:\n%s", out)
	}

	goFile := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(goFile, out, 0o644); err != nil {
		t.Fatal(err)
	}
	binFile := filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command("go", "build", "-o", binFile, goFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v\n%s", err, stderr.String())
	}

	// Prove the no-crash property end-to-end: run a program where `stop`
	// is the only thing alive (the worst case for select{}, which the Go
	// runtime would kill almost instantly here) and confirm it's still
	// running, not crashed, after a short wait.
	soloSrc := []byte("package main\n\nfunc main() {\n\tstop\n}\n")
	soloOut, err := Transform(soloSrc)
	if err != nil {
		t.Fatalf("Transform (solo): %v", err)
	}
	soloGoFile := filepath.Join(t.TempDir(), "solo.go")
	if err := os.WriteFile(soloGoFile, soloOut, 0o644); err != nil {
		t.Fatal(err)
	}
	soloBin := filepath.Join(t.TempDir(), "solo")
	if out, err := exec.Command("go", "build", "-o", soloBin, soloGoFile).CombinedOutput(); err != nil {
		t.Fatalf("go build (solo): %v\n%s", err, out)
	}

	runCmd := exec.Command(soloBin)
	if err := runCmd.Start(); err != nil {
		t.Fatalf("start solo: %v", err)
	}
	defer runCmd.Process.Kill()

	done := make(chan error, 1)
	go func() { done <- runCmd.Wait() }()

	select {
	case err := <-done:
		t.Fatalf("solo process exited/crashed instead of blocking forever: %v", err)
	case <-time.After(500 * time.Millisecond):
		// still running after 500ms, as expected
		runCmd.Process.Kill()
	}
}

// TestLinkRewrite checks that `link[idx]` sends/receives rewrite to
// bilink calls, with the bilink import auto-injected the same way
// `stop` pulls in "time" and `altN` pulls in "reflect". It can't use
// the TestOK ok/*.bil shape: the generated code imports
// emulator/nodeprog/bilink, a different Go module this one doesn't
// depend on (see ../../../emulator, a sibling repo) and which is
// //go:build js && wasm-gated besides — building it here, on the host
// arch, can't work at all. Checking the transformed text is the right
// level of check; see emulator/README.md for how this actually runs.
func TestLinkRewrite(t *testing.T) {
	src := []byte(`package main

proc node() {
	var v int32
	link[west] -> v
	link[east] <- v
}

func main() {
	node()
}
`)
	out, err := Transform(src)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "v = bilink.Recv(west)") {
		t.Errorf("expected `link[west] -> v` to rewrite to `v = bilink.Recv(west)`, got:\n%s", got)
	}
	if !strings.Contains(got, "bilink.Send(east, v)") {
		t.Errorf("expected `link[east] <- v` to rewrite to `bilink.Send(east, v)`, got:\n%s", got)
	}
	if !strings.Contains(got, `import "emulator/nodeprog/bilink"`) {
		t.Errorf("expected the bilink import to be auto-injected, got:\n%s", got)
	}

	// A source that already imports bilink itself shouldn't get a second,
	// duplicate import — same convention checkNoBufferedChannels/usedStop
	// etc. already follow via hasImport.
	srcWithImport := []byte(`package main

import "emulator/nodeprog/bilink"

proc node() {
	var v int32
	link[west] -> v
}

func main() {
	node()
}
`)
	out2, err := Transform(srcWithImport)
	if err != nil {
		t.Fatalf("Transform (pre-imported): %v", err)
	}
	if n := strings.Count(string(out2), `"emulator/nodeprog/bilink"`); n != 1 {
		t.Errorf("expected exactly one bilink import, got %d in:\n%s", n, out2)
	}
}

// The timer exemption ("a timer may be used for input by any number of
// components of a parallel") no longer has a bilc side to test here —
// bilc's own point-to-point channel check has been removed entirely; the
// exemption is tested where the rule itself now lives, at
// tools/static_check/testdata/chan_timer_multi_recv.go.
