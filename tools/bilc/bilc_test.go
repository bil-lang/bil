package bilc

import (
	"bytes"
	"fmt"
	"go/token"
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
			out, err := Transform(bilFile, src)
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

			_, err = Transform(bilFile, src)
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
// tools/vet's variables.go/channels.go, which — unlike the bilc
// checks they replaced — aren't restricted to call-shaped branches, so a
// bare `{...}` or `seq{...}` branch closing over and mutating a variable
// directly (both legal shapes, sanctioned by Example 1's design notes) is
// no longer even a residual gap *there* — vet does catch this
// example, confirmed directly). This test proves TestOK's `-race` pass
// does, as bilc's own dynamic backstop for bilc's own now-total absence of
// a static check here — not a fix, and not sound (a race the detector
// doesn't happen to hit on a given run still slips through), but real
// coverage for bilc's real hole, one vet itself has already
// closed for anyone running the combined `bil run` pipeline rather
// than bare `bilc` — bilc's own checks have been progressively removed in
// vet's favor.
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
	out, err := Transform("test.bil", src)
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
	out, err := Transform("test.bil", src)
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
	soloOut, err := Transform("test.bil", soloSrc)
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
	out, err := Transform("test.bil", src)
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
	out2, err := Transform("test.bil", srcWithImport)
	if err != nil {
		t.Fatalf("Transform (pre-imported): %v", err)
	}
	if n := strings.Count(string(out2), `"emulator/nodeprog/bilink"`); n != 1 {
		t.Errorf("expected exactly one bilink import, got %d in:\n%s", n, out2)
	}
}

// TestPlacedParRewrite checks that `placed par` rewrites to a plain Go
// `switch`, exercising both `processor(...)` arities (flat ID and 2D) side
// by side in one block, plus `default`, and that it triggers the same
// build-tag/bilink-import auto-injection `link[...]` does (see
// TestLinkRewrite) — `placed par` calls into bilink for
// Row()/Col()/ID() even though it never writes `link[...]` itself.
func TestPlacedParRewrite(t *testing.T) {
	src := []byte(`package main

proc boot() {
	println("boot")
}

proc controller() {
	println("controller")
}

proc relay() {
	println("relay")
}

func main() {
	placed par {
		processor(0) {
			boot()
		}
		processor(0, 0) {
			controller()
		}
		default {
			relay()
		}
	}
}
`)
	out, err := Transform("test.bil", src)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"switch {",
		"case bilink.ID() == 0:",
		"case bilink.Row() == 0 && bilink.Col() == 0:",
		"default:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output, got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "//go:build js && wasm") {
		t.Errorf("expected the js/wasm build tag to be auto-injected, got:\n%s", got)
	}
	if n := strings.Count(got, `"emulator/nodeprog/bilink"`); n != 1 {
		t.Errorf("expected exactly one bilink import, got %d in:\n%s", n, got)
	}
}

// TestPlacedParDoesNotAffectOrdinaryPar combines an ordinary `par{...}`
// and a `placed par{...}` in the same file — "placed" and "par" are
// distinct token literals with no shape either construct's case in
// transform() could mistake for the other's (see the design notes in
// bilc.go), so this is a sanity check, not a regression test for a bug:
// unlike an earlier, now-superseded design (bil-occamy's PLACEMENT-DESIGN.md,
// written against a different, token-substring-based checker this
// codebase doesn't have), there's no confirmed bug here to guard against.
func TestPlacedParDoesNotAffectOrdinaryPar(t *testing.T) {
	src := []byte(`package main

proc worker(c chan<- int) {
	c <- 1
}

proc controller() {
	println("controller")
}

proc idle() {
	println("default")
}

func main() {
	c := make(chan int)
	par {
		worker(c)
		seq {
			var v int
			c -> v
			println(v)
		}
	}

	placed par {
		processor(0) {
			controller()
		}
		default {
			idle()
		}
	}
}
`)
	out, err := Transform("test.bil", src)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "par(\n") {
		t.Errorf("expected the ordinary par{} to still rewrite to par(...), got:\n%s", got)
	}
	if !strings.Contains(got, "switch {") {
		t.Errorf("expected the placed par{} to rewrite to switch {, got:\n%s", got)
	}
}

// TestPlaceAliasRewrite mirrors TestLinkRewrite, but for the call-site
// form: `place NAME at link[EXPR]` now lives in the placed-par clause that
// calls the proc, binding one of its own declared directional channel
// parameters -- toEast/fromEast here -- rather than minting a name inside
// the proc's own body. The proc itself never mentions `place`/`link` at
// all; every trace of the call-site's local place names (toEast/fromEast)
// is gone from the output too, resolved straight to the link indices they
// stood for, exactly as if the proc's own body had `link[east]` written
// directly -- and the callee's own parameter names (in, out) are what
// survive into the emitted bilink calls.
func TestPlaceAliasRewrite(t *testing.T) {
	src := []byte(`package main

proc node(in <-chan int32, out chan<- int32) {
	var v int32
	in -> v
	out <- v
}

func main() {
	placed par {
		processor(0) {
			place toEast at link[east]
			place fromEast at link[east]
			node(fromEast, toEast)
		}
	}
}
`)
	out, err := Transform("test.bil", src)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "v = bilink.Recv(east)") {
		t.Errorf("expected `in -> v` to rewrite to `v = bilink.Recv(east)`, got:\n%s", got)
	}
	if !strings.Contains(got, "bilink.Send(east, v)") {
		t.Errorf("expected `out <- v` to rewrite to `bilink.Send(east, v)`, got:\n%s", got)
	}
	if strings.Contains(got, "toEast") || strings.Contains(got, "fromEast") {
		t.Errorf("expected the place declarations and every call-site use of their names to be rewritten away, got:\n%s", got)
	}
	if !strings.Contains(got, "node(nil, nil)") {
		t.Errorf("expected the placed call's channel arguments to become nil, got:\n%s", got)
	}
	if !strings.Contains(got, `import "emulator/nodeprog/bilink"`) {
		t.Errorf("expected the bilink import to be auto-injected, got:\n%s", got)
	}
}

// TestPlacementManifest exercises the topology manifest against a fixture
// using both `processor` arities plus a `default` (with an if/else body,
// so it's recorded with no `proc:` line — see PlacementManifest's own
// doc comment for why that's correct, not a gap) and two placed call
// sites, and confirms an ordinary (non-`placed par`) file gets no manifest
// at all rather than an empty one. The `links: aliases:` section is keyed
// by each callee's own parameter name (toEast, toWest), not by the
// call-site's local place name — see PlacementManifest's own doc comment.
func TestPlacementManifest(t *testing.T) {
	src := []byte(`package main

proc controller(toEast chan<- int32) {
	toEast <- 1
}

proc rowEnd(toWest chan<- int32) {
	toWest <- 1
}

proc relay() {
	println("relay")
}

proc idle() {
	println("idle")
}

func main() {
	r, cols := 0, 4
	placed par {
		processor(0, 0) {
			place toEast at link[1]
			controller(toEast)
		}
		processor(0, cols-1) {
			place toWest at link[3]
			rowEnd(toWest)
		}
		default {
			if r == 0 {
				relay()
			} else {
				idle()
			}
		}
	}
}
`)
	m, err := PlacementManifest("test.bil", src)
	if err != nil {
		t.Fatalf("PlacementManifest: %v", err)
	}
	got := string(m)
	for _, want := range []string{
		"match: {row: 0, col: 0}",
		"proc: controller",
		`match: {row: 0, col: "cols-1"}`,
		"proc: rowEnd",
		"default:",
		"toEast: 1",
		"toWest: 3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in manifest, got:\n%s", want, got)
		}
	}
	// default's body is an if/else, not a single call -- it must not be
	// credited with a resolved proc name it doesn't actually have.
	if strings.Contains(got, "proc: relay") || strings.Contains(got, "proc: idle") {
		t.Errorf("expected default's unresolved if/else body to have no proc: line, got:\n%s", got)
	}

	noPlacement := []byte(`package main

func main() {
	println("hello")
}
`)
	m2, err := PlacementManifest("test.bil", noPlacement)
	if err != nil {
		t.Fatalf("PlacementManifest (no placed par): %v", err)
	}
	if m2 != nil {
		t.Errorf("expected nil manifest for a file with no placed par block, got:\n%s", m2)
	}
}

// TestPlacedCallSiteConstantParam checks that a placed-callable proc may
// take a genuine compile-time-constant parameter (a literal, or a
// reference to a package-level const) alongside its channel parameters --
// the constant's source text is copied through unchanged at the call
// site, while the channel argument still becomes nil.
func TestPlacedCallSiteConstantParam(t *testing.T) {
	src := []byte(`package main

const rounds = 3

proc controller(n int, toEast chan<- int32) {
	for i := 0; i < n; i++ {
		toEast <- int32(i)
	}
}

func main() {
	placed par {
		processor(0) {
			place toEast at link[1]
			controller(rounds, toEast)
		}
	}
}
`)
	out, err := Transform("test.bil", src)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "controller(rounds, nil)") {
		t.Errorf("expected the constant argument to pass through and the channel argument to become nil, got:\n%s", got)
	}
	if !strings.Contains(got, "bilink.Send(1, int32(i))") {
		t.Errorf("expected `toEast <- int32(i)` to rewrite to `bilink.Send(1, int32(i))`, got:\n%s", got)
	}
}

// TestPlacedCallSiteIfElseLeaves checks that a placed-par clause may
// dispatch between two different placed calls via if/else (the shape
// examples 19-21's own `default { if r == 0 { relay() } else { idle() } }`
// idiom needs), each leaf with its own independent place bindings.
func TestPlacedCallSiteIfElseLeaves(t *testing.T) {
	src := []byte(`package main

proc relay(in <-chan int32, out chan<- int32) {
	var v int32
	in -> v
	out <- v
}

proc idle() {
	println("idle")
}

func main() {
	r := 0
	placed par {
		default {
			if r == 0 {
				place in at link[3]
				place out at link[1]
				relay(in, out)
			} else {
				idle()
			}
		}
	}
}
`)
	out, err := Transform("test.bil", src)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		// "if" and its condition land on separate lines: the condition is
		// re-run through t.transform, which resyncs with its own `//line`
		// directive per source line (see resync in bilc.go) -- checked
		// separately rather than as one adjacent substring for that reason.
		"if\n",
		"r == 0 {",
		"relay(nil, nil)",
		"v = bilink.Recv(3)",
		"bilink.Send(1, v)",
		"} else {",
		"idle()",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output, got:\n%s", want, got)
		}
	}
}

// TestIsConstExpr exercises isConstExpr's whitelist directly: literals and
// references to a package-level const must pass; a variable reference, a
// call/conversion shape, and a selector must all fail.
func TestIsConstExpr(t *testing.T) {
	src := []byte(`package main

const rounds = 3

func main() {
	x := rounds + 1*2
	y := bilink.NumCols()
	z := rounds
	w := pkg.Const
}
`)
	tr := tokenize("test.bil", src)
	constNames := tr.collectTopLevelConstNames()
	if !constNames["rounds"] {
		t.Fatalf("expected 'rounds' to be collected as a top-level const")
	}

	// Locate each RHS by its DEFINE token (`x :=`, `y :=`, ...) and run to
	// the following SEMICOLON.
	rhsFor := func(name string) (lo, hi int) {
		for i, tk := range tr.toks {
			if tk.tok == token.IDENT && tk.lit == name && i+1 < len(tr.toks) && tr.toks[i+1].tok == token.DEFINE {
				lo = i + 2
				for j := lo; j < len(tr.toks); j++ {
					if tr.toks[j].tok == token.SEMICOLON {
						return lo, j
					}
				}
			}
		}
		t.Fatalf("could not find %q :=", name)
		return 0, 0
	}

	if lo, hi := rhsFor("x"); !tr.isConstExpr(lo, hi, constNames) {
		t.Errorf("expected `rounds + 1*2` to be a const expr")
	}
	if lo, hi := rhsFor("y"); tr.isConstExpr(lo, hi, constNames) {
		t.Errorf("expected `bilink.NumCols()` to NOT be a const expr")
	}
	if lo, hi := rhsFor("z"); !tr.isConstExpr(lo, hi, constNames) {
		t.Errorf("expected `rounds` to be a const expr")
	}
	if lo, hi := rhsFor("w"); tr.isConstExpr(lo, hi, constNames) {
		t.Errorf("expected `pkg.Const` to NOT be a const expr")
	}
}

// TestLineDirectives checks that Transform's `//line` directives (see
// resync in bilc.go) correctly map error positions in the transpiled Go
// back to the original .bil file and line — the mechanism tools/vet and
// `go run` both rely on to report positions in terms of .bil source
// rather than the generated Go. The guarded alt here injects several
// lines of synthetic setup (bilGuard0 := ...; if !cond {...}; select
// {...}) ahead of the underlying select, which is exactly the kind of
// line-count drift resync exists to correct for; the fixture's line
// numbers are load-bearing (see the inline comments) — if this literal
// source string is ever reformatted, wantLine must be updated to match.
func TestLineDirectives(t *testing.T) {
	const filename = "line-directive-test.bil"
	src := []byte(`package main

func main() {
	q := 5
	c := make(chan int)

	alt {
		(q > 0) && c :-> v {
			println(v)
		}
		skip {
			println("skip")
		}
	}

	println("after")
}
`)
	const wantLine = 16 // the "println(\"after\")" line, above

	out, err := Transform(filename, src)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	abs, err := filepath.Abs(filename)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("//line %s:%d\n\tprintln(\"after\")", abs, wantLine)
	got := string(out)
	if !strings.Contains(got, want) {
		t.Errorf("expected %q immediately before the post-alt statement, got:\n%s", want, got)
	}
}

// The timer exemption ("a timer may be used for input by any number of
// components of a parallel") no longer has a bilc side to test here —
// bilc's own point-to-point channel check has been removed entirely; the
// exemption is tested where the rule itself now lives, at
// tools/vet/testdata/chan_timer_multi_recv.go.
