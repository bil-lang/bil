package bilc

import (
	"bytes"
	"fmt"
	"go/format"
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
// emulator/bilink, a different Go module this one doesn't
// depend on (see ../../../emulator, a sibling repo) — building it
// here can't work at all regardless of platform. Checking the
// transformed text is the right level of check; see
// emulator/README.md for how this actually runs (in-browser via WASM,
// or natively via cmd/multicore).
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
	if !strings.Contains(got, `import "emulator/bilink"`) {
		t.Errorf("expected the bilink import to be auto-injected, got:\n%s", got)
	}

	// A source that already imports bilink itself shouldn't get a second,
	// duplicate import — same convention checkNoBufferedChannels/usedStop
	// etc. already follow via hasImport.
	srcWithImport := []byte(`package main

import "emulator/bilink"

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
	if n := strings.Count(string(out2), `"emulator/bilink"`); n != 1 {
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
		processor(*, *) {
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
	if strings.Contains(got, "//go:build") {
		// bilink itself owns the platform split (js/wasm vs. native --
		// see emulator/bilink/bilink.go and bilink_native.go), so
		// generated code that merely calls into it must stay
		// build-tag-free to be buildable under either.
		t.Errorf("expected no build tag on generated code (bilink owns that split), got:\n%s", got)
	}
	if n := strings.Count(got, `"emulator/bilink"`); n != 1 {
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
		processor(*) {
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
	if !strings.Contains(got, `import "emulator/bilink"`) {
		t.Errorf("expected the bilink import to be auto-injected, got:\n%s", got)
	}
}

// TestPlaceDeclInOutSuffix checks that `place NAME at link[EXPR].in` and
// `.out` parse identically to the bare `link[EXPR]` form -- the suffix
// is documentation only, discarded during codegen, never cross-checked
// against the callee's declared parameter direction (see
// parsePlaceDecl's own doc comment for why).
func TestPlaceDeclInOutSuffix(t *testing.T) {
	src := []byte(`package main

proc node(in <-chan int32, out chan<- int32) {
	var v int32
	in -> v
	out <- v
}

func main() {
	placed par {
		processor(0) {
			place out2 at link[1].out
			place in2 at link[1].in
			node(in2, out2)
		}
	}
}
`)
	out, err := Transform("test.bil", src)
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "v = bilink.Recv(1)") {
		t.Errorf("expected `in -> v` to rewrite to `v = bilink.Recv(1)`, got:\n%s", got)
	}
	if !strings.Contains(got, "bilink.Send(1, v)") {
		t.Errorf("expected `out <- v` to rewrite to `bilink.Send(1, v)`, got:\n%s", got)
	}
	if strings.Contains(got, ".in") || strings.Contains(got, ".out") {
		t.Errorf("expected the .in/.out suffix to be discarded entirely, got:\n%s", got)
	}
}

// TestRoleBinaries checks that RoleBinaries emits one standalone,
// syntactically valid Go file per distinct placed-par role, each with
// its own main() calling only that role's proc -- and that every
// clause-match/leaf-condition expression is discard-referenced even in
// a role (like idle, reached only via the if/else's else side) whose
// own call never uses those identifiers, so Go doesn't reject r/cols as
// "declared and not used" once the switch/if-else that used to
// reference them is collapsed away.
func TestRoleBinaries(t *testing.T) {
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
		processor(*, *) {
			if r == 0 {
				relay()
			} else {
				idle()
			}
		}
	}
}
`)
	roles, err := RoleBinaries("test.bil", src)
	if err != nil {
		t.Fatalf("RoleBinaries: %v", err)
	}
	wantRoles := []string{"controller", "rowEnd", "relay", "idle"}
	if len(roles) != len(wantRoles) {
		t.Fatalf("expected %d roles, got %d: %v", len(wantRoles), len(roles), roles)
	}
	for _, name := range wantRoles {
		code, ok := roles[name]
		if !ok {
			t.Fatalf("missing role %q among: %v", name, roles)
		}
		got := string(code)
		if !strings.Contains(got, "func main() {") {
			t.Errorf("role %q: expected a main() func, got:\n%s", name, got)
		}
		if !strings.Contains(got, name+"(") {
			t.Errorf("role %q: expected main() to call %s(...), got:\n%s", name, name, got)
		}
		for _, discard := range []string{"_ = (0)", "_ = (cols - 1)", "_ = (r == 0)"} {
			if !strings.Contains(got, discard) {
				t.Errorf("role %q: expected %q to keep r/cols referenced, got:\n%s", name, discard, got)
			}
		}
		if _, err := format.Source(code); err != nil {
			t.Errorf("role %q: not valid Go: %v\n%s", name, err, got)
		}
	}

	noPlacement := []byte(`package main

func main() {
	println("hello")
}
`)
	roles2, err := RoleBinaries("test.bil", noPlacement)
	if err != nil {
		t.Fatalf("RoleBinaries (no placed par): %v", err)
	}
	if roles2 != nil {
		t.Errorf("expected nil roles for a file with no placed par block, got: %v", roles2)
	}
}

// TestDeployManifest checks the JSON deployment manifest's shape: each
// leaf's clause match, its ordered if/else condition chain (with
// negate set for the else side), the resolved proc, and its link-index
// binds keyed by the callee's own declared parameter names.
func TestDeployManifest(t *testing.T) {
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
		processor(*, *) {
			if r == 0 {
				relay()
			} else {
				idle()
			}
		}
	}
}
`)
	m, err := DeployManifest("test.bil", src)
	if err != nil {
		t.Fatalf("DeployManifest: %v", err)
	}
	got := string(m)
	for _, want := range []string{
		`"transport": "message-channel"`,
		`"row": "0"`,
		`"col": "0"`,
		`"proc": "controller"`,
		`"toEast": "1"`,
		`"col": "cols-1"`,
		`"proc": "rowEnd"`,
		`"toWest": "3"`,
		`"default": true`,
		`"expr": "r == 0"`,
		`"negate": false`,
		`"negate": true`,
		`"proc": "relay"`,
		`"proc": "idle"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in deploy manifest, got:\n%s", want, got)
		}
	}

	noPlacement := []byte(`package main

func main() {
	println("hello")
}
`)
	m2, err := DeployManifest("test.bil", noPlacement)
	if err != nil {
		t.Fatalf("DeployManifest (no placed par): %v", err)
	}
	if m2 != nil {
		t.Errorf("expected nil manifest for a file with no placed par block, got:\n%s", m2)
	}
}

// TestPlacedParWildcard exercises `processor(...)`'s wildcard forms
// across all three consumers that parse a placed-par block: Transform's
// switch codegen, DeployManifest's JSON, and RoleBinaries' per-role Go.
// The fixture uses `processor(1, *)` (row pinned, column wild) and
// `processor(*, *)` (fully wild, replacing an explicit `default`) side
// by side with an ordinary exact-node clause, so each consumer's
// wildcard-vs-exact handling can be checked in one place.
func TestPlacedParWildcard(t *testing.T) {
	src := []byte(`package main

proc controller(toEast chan<- int32) {
	toEast <- 1
}

proc relay() {
	println("relay")
}

proc idle() {
	println("idle")
}

func main() {
	placed par {
		processor(1, 0) {
			place toEast at link[1]
			controller(toEast)
		}
		processor(1, *) {
			relay()
		}
		processor(*, *) {
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
	for _, want := range []string{
		"case bilink.Row() == 1 && bilink.Col() == 0:",
		"case bilink.Row() == 1:",
		"default:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in transformed output, got:\n%s", want, got)
		}
	}
	// processor(*, *) must compile to Go's own `default:`, never a
	// literal `*` spliced into a comparison -- that would be invalid Go.
	if strings.Contains(got, "== *") || strings.Contains(got, "* ==") {
		t.Errorf("expected the wildcard sentinel to never reach a comparison, got:\n%s", got)
	}

	dm, err := DeployManifest("test.bil", src)
	if err != nil {
		t.Fatalf("DeployManifest: %v", err)
	}
	gotDM := string(dm)
	for _, want := range []string{
		`"row": "1"`,
		`"col": "0"`,
		`"proc": "controller"`,
		`"col": "*"`,
		`"proc": "relay"`,
		`"default": true`,
		`"proc": "idle"`,
	} {
		if !strings.Contains(gotDM, want) {
			t.Errorf("expected %q in deploy manifest, got:\n%s", want, gotDM)
		}
	}

	roles, err := RoleBinaries("test.bil", src)
	if err != nil {
		t.Fatalf("RoleBinaries: %v", err)
	}
	for _, name := range []string{"controller", "relay", "idle"} {
		role, ok := roles[name]
		if !ok {
			t.Fatalf("expected a %q role binary, got roles: %v", name, roles)
		}
		if _, err := format.Source(role); err != nil {
			t.Errorf("role %q not valid Go: %v\n%s", name, err, role)
		}
	}
}

// TestPlacedParWildcardMustBeLast checks that a fully-wild clause
// (`processor(*)`/`processor(*, *)`) is rejected unless it's the block's
// last clause -- the same "default must be last" rule an explicit
// `default` already enforces, since both mean "matches every node" and
// a clause after one can never be reached.
func TestPlacedParWildcardMustBeLast(t *testing.T) {
	src := []byte(`package main

proc idle() {
	println("idle")
}

proc controller() {
	println("controller")
}

func main() {
	placed par {
		processor(*, *) {
			idle()
		}
		processor(0, 0) {
			controller()
		}
	}
}
`)
	if _, err := Transform("test.bil", src); err == nil {
		t.Fatalf("expected an error for a clause following a fully-wild processor(*, *), got none")
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
// dispatch between two different placed calls via if/else -- still a
// valid shape (each leaf independently validated and given its own
// place bindings) even though examples 20/21 no longer need it
// themselves, having moved to explicit `processor(R, *)` wildcard
// clauses instead of branching inside one catch-all.
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
		processor(*, *) {
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
