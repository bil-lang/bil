package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"

	"bilc"
	"vet"
)

// emuMain parses `bil emu`'s flags and calls emuCmd. Kept separate from
// emuCmd so the latter stays a plain, directly-testable function with no
// flag.FlagSet involved, matching runCmd's own shape.
func emuMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("emu", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// No backtick-quoted words in these usage strings -- flag.PrintDefaults
	// treats a single back-quoted substring as the flag's value-type name
	// (e.g. would print "-cols placed par" instead of "-cols int").
	target := fs.String("target", "wasm", "execution backend: wasm (browser/WASM grid, "+
		"emulator/cmd/wasm) or multicore (native OS-process-per-core grid, emulator/cmd/multicore)")
	rows := fs.Int("rows", 0, "grid rows (0 = auto: 1 for a plain chan/proc/par program,"+
		" or the smallest grid that gives every placed processor its own node"+
		" for a placed-par program -- see -cols)")
	cols := fs.Int("cols", 0, "grid cols (0 = auto, see -rows). NOTE: forcing a placed-par"+
		" program onto too small a grid can silently under-exercise it -- e.g."+
		" processor(0,0) and processor(0,cols-1) collide into the same generated"+
		" Go switch case when cols=1, and only the first match ever fires")
	addr := fs.String("addr", "localhost:8787", "wasm target only: address for the emulator's static file server to listen on")
	open := fs.Bool("open", true, "wasm target only: open the default browser once the server is ready")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bil emu [-target wasm|multicore] [-rows N] [-cols N] [-addr host:port] [-open] <file.bil>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if *target != "wasm" && *target != "multicore" {
		fmt.Fprintf(stderr, "bil emu: -target must be \"wasm\" or \"multicore\", got %q\n", *target)
		return 2
	}
	return emuCmd(fs.Arg(0), *target, *rows, *cols, *addr, *open, stdout, stderr)
}

// emuCmd transpiles src with bilc, vets it (refusing to proceed on any
// violation, exactly like runCmd's execute=true path), detects placement
// via bilc.RoleBinaries/PlacementManifest, and hands the built role sources
// to one of two execution backends at a sibling ../emulator checkout
// (see emulator/README.md's Install section for that convention, which
// this reuses rather than inventing an env var or config override):
// "wasm" (the default, matching this command's original and only
// behavior) runs them in the browser/WASM grid; "multicore" runs them as
// real native OS processes, one per CPU core, via emulator/cmd/multicore.
// Both share everything through transpiling and writing role sources to
// a scratch dir -- see runWasmTarget/runMulticoreTarget for where they
// actually diverge.
func emuCmd(src, target string, rows, cols int, addr string, openBrowser bool, stdout, stderr io.Writer) int {
	in, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	out, err := bilc.Transform(src, in)
	if err != nil {
		fmt.Fprintln(stderr, "transform error:", err)
		return 1
	}

	roles, err := bilc.RoleBinaries(src, in)
	if err != nil {
		fmt.Fprintln(stderr, "role split error:", err)
		return 1
	}

	emulatorDir, err := findEmulatorDir()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	// A non-placed program has no emulator/bilink import to resolve at
	// all, so it's checked here, immediately, exactly like runCmd's own
	// execute=true path -- refusing to proceed on any violation before
	// ever touching the emulator checkout. A placed/link program is
	// checked further down instead, once its role sources are written
	// (see the vet.Check loop after they're split out): each role's own
	// scratch directory already lives inside the real emulator module
	// (nested under emulatorDir/nodeprog), so emulator/bilink resolves
	// there via the module's own ordinary self-import -- no generated
	// go.mod/replace needed the way runCmd needs one for its own,
	// unrelated bare temp file.
	if roles == nil {
		tmp, err := os.CreateTemp("", "bil-emu-*.go")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(out); err != nil {
			tmp.Close()
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := tmp.Close(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		messages, err := vet.Check(tmp.Name())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if len(messages) > 0 {
			for _, m := range messages {
				fmt.Fprintln(stderr, m)
			}
			fmt.Fprintf(stderr, "\nbil: static analysis failed for %s — not running (see violation(s) above)\n", src)
			return 1
		}
	}

	if rows == 0 || cols == 0 {
		if roles == nil {
			rows, cols = 1, 1
		} else {
			manifest, err := bilc.PlacementManifest(src, in)
			if err != nil {
				fmt.Fprintln(stderr, "placement manifest error:", err)
				return 1
			}
			inferredRows, inferredCols, err := inferGridSize(manifest)
			if err != nil {
				fmt.Fprintln(stderr, "inferring grid size:", err)
				return 1
			}
			if rows == 0 {
				rows = inferredRows
			}
			if cols == 0 {
				cols = inferredCols
			}
		}
	}

	// scratchDir holds the transpiled role source(s) -- plain Go, not yet
	// built for any particular target -- exactly the on-disk shape
	// emulator/README.md's own Quickstart produces by hand
	// (nodeprog/<name>/main.go, or roles/*/main.go + roles/placement.json).
	// It lives under emulatorDir/nodeprog so it resolves the "emulator"
	// module's own go.mod, the same reason emulator/README.md's Quickstart
	// always builds from inside the emulator checkout.
	scratchDir, err := os.MkdirTemp(filepath.Join(emulatorDir, "nodeprog"), "bilemu-*")
	if err != nil {
		fmt.Fprintln(stderr, "creating build scratch dir:", err)
		return 1
	}
	defer os.RemoveAll(scratchDir)

	if roles == nil {
		if err := os.WriteFile(filepath.Join(scratchDir, "main.go"), out, 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	} else {
		manifest, err := bilc.PlacementManifest(src, in)
		if err != nil {
			fmt.Fprintln(stderr, "placement manifest error:", err)
			return 1
		}
		for role, roleSrc := range roles {
			roleScratch := filepath.Join(scratchDir, "roles", role)
			if err := os.MkdirAll(roleScratch, 0o755); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			if err := os.WriteFile(filepath.Join(roleScratch, "main.go"), roleSrc, 0o644); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		if err := os.WriteFile(filepath.Join(scratchDir, "roles", "placement.json"), manifest, 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}

		// Every role's own scratch directory (written just above) already
		// sits inside the real emulator module and holds exactly one
		// file, so vet.CheckInPlace -- unlike Check -- needs no generated
		// go.mod/replace and no staging elsewhere: it resolves
		// emulator/bilink straight from where the file already is. See
		// this function's own doc comment on the roles == nil branch
		// above for why that's different from runCmd's bare-temp-file
		// case. Sorted for deterministic message ordering across runs.
		roleNames := make([]string, 0, len(roles))
		for role := range roles {
			roleNames = append(roleNames, role)
		}
		sort.Strings(roleNames)
		var anyViolations bool
		for _, role := range roleNames {
			roleMain := filepath.Join(scratchDir, "roles", role, "main.go")
			messages, err := vet.CheckInPlace(roleMain)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			for _, m := range messages {
				fmt.Fprintln(stderr, m)
				anyViolations = true
			}
		}
		if anyViolations {
			fmt.Fprintf(stderr, "\nbil: static analysis failed for %s — not running (see violation(s) above)\n", src)
			return 1
		}
	}

	if target == "multicore" {
		return runMulticoreTarget(emulatorDir, scratchDir, src, rows, cols, stdout, stderr)
	}
	return runWasmTarget(emulatorDir, scratchDir, roles != nil, src, rows, cols, addr, openBrowser, stdout, stderr)
}

// runWasmTarget builds scratchDir's role source(s) to WASM, assembles a
// servable directory (emulator/cmd/wasm's static assets + the built
// .wasm(es) + roles/placement.json when placed), and runs
// emulator/cmd/wasm to serve it, opening a browser -- this is `bil emu`'s
// original and, until -target existed, only behavior.
func runWasmTarget(emulatorDir, scratchDir string, placed bool, src string, rows, cols int, addr string, openBrowser bool, stdout, stderr io.Writer) int {
	serveDir, err := os.MkdirTemp("", "bil-emu-serve-*")
	if err != nil {
		fmt.Fprintln(stderr, "creating serve dir:", err)
		return 1
	}
	defer os.RemoveAll(serveDir)

	for _, name := range []string{"index.html", "node-worker.js", "wasm_exec.js"} {
		if err := copyFile(filepath.Join(emulatorDir, "cmd", "wasm", "static", name), filepath.Join(serveDir, name)); err != nil {
			fmt.Fprintln(stderr, "copying static assets:", err)
			return 1
		}
	}

	if !placed {
		if err := goBuildWasm(scratchDir, filepath.Join(serveDir, "node.wasm")); err != nil {
			fmt.Fprintln(stderr, "building node.wasm:", err)
			return 1
		}
	} else {
		rolesDir := filepath.Join(serveDir, "roles")
		if err := os.MkdirAll(rolesDir, 0o755); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		entries, err := os.ReadDir(filepath.Join(scratchDir, "roles"))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			role := e.Name()
			if err := goBuildWasm(filepath.Join(scratchDir, "roles", role), filepath.Join(rolesDir, role+".wasm")); err != nil {
				fmt.Fprintf(stderr, "building role %q: %v\n", role, err)
				return 1
			}
		}
		if err := copyFile(filepath.Join(scratchDir, "roles", "placement.json"), filepath.Join(rolesDir, "placement.json")); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}

	// Build emulator/cmd/wasm once and exec the binary directly, rather
	// than `go run ./cmd/wasm`: go run spawns the actual compiled binary
	// as its own child process and does not forward a kill signal to it
	// -- confirmed directly (SIGINT-ing `go run` leaves the real serve
	// process orphaned, still holding the port). Killing a directly-run
	// binary via the context has no such gap.
	serveBinFile, err := os.CreateTemp("", "bil-emu-serve-bin-*")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	serveBin := serveBinFile.Name()
	serveBinFile.Close()
	defer os.Remove(serveBin)
	buildServe := exec.Command("go", "build", "-o", serveBin, "./cmd/wasm")
	buildServe.Dir = emulatorDir
	if output, err := buildServe.CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "building emulator's cmd/wasm: %v\n%s", err, output)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveCmd := exec.CommandContext(ctx, serveBin, "-dir", serveDir, "-addr", addr)
	serveCmd.Stdout = stdout
	serveCmd.Stderr = stderr
	if err := serveCmd.Start(); err != nil {
		fmt.Fprintln(stderr, "starting emulator server:", err)
		return 1
	}

	url := fmt.Sprintf("http://%s/?rows=%d&cols=%d", addr, rows, cols)
	fmt.Fprintf(stdout, "bil emu: serving %s at %s (Ctrl-C to stop)\n", src, url)
	if openBrowser {
		if err := openInBrowser(url); err != nil {
			fmt.Fprintln(stderr, "bil emu: couldn't auto-open a browser:", err)
		}
	}

	return waitAndTranslateExit(serveCmd, ctx, stderr)
}

// runMulticoreTarget builds emulator/cmd/multicore (which builds
// scratchDir's role source(s) natively itself -- no WASM step at all)
// and runs it directly against scratchDir, one real OS process per grid
// node. There's no server or browser step for this target: each process
// prints its own prefixed output directly to stdout/stderr, the same
// prefixed-console model emulator/cmd/multicore/README.md documents.
func runMulticoreTarget(emulatorDir, scratchDir, src string, rows, cols int, stdout, stderr io.Writer) int {
	if n := runtime.NumCPU(); rows*cols > n {
		fmt.Fprintf(stderr, "bil emu: -target multicore needs a %dx%d grid (%d processes), but this machine has only %d CPU cores -- pass smaller -rows/-cols\n", rows, cols, rows*cols, n)
		return 1
	}

	multicoreBinFile, err := os.CreateTemp("", "bil-emu-multicore-bin-*")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	multicoreBin := multicoreBinFile.Name()
	multicoreBinFile.Close()
	defer os.Remove(multicoreBin)
	buildMulticore := exec.Command("go", "build", "-o", multicoreBin, "./cmd/multicore")
	buildMulticore.Dir = emulatorDir
	if output, err := buildMulticore.CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "building emulator's cmd/multicore: %v\n%s", err, output)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(stdout, "bil emu: running %s natively, %dx%d grid (Ctrl-C to stop)\n", src, rows, cols)
	multicoreCmd := exec.CommandContext(ctx, multicoreBin,
		"-dir", scratchDir,
		"-rows", fmt.Sprint(rows),
		"-cols", fmt.Sprint(cols),
	)
	multicoreCmd.Stdout = stdout
	multicoreCmd.Stderr = stderr
	if err := multicoreCmd.Start(); err != nil {
		fmt.Fprintln(stderr, "starting multicore emulator:", err)
		return 1
	}

	return waitAndTranslateExit(multicoreCmd, ctx, stderr)
}

// waitAndTranslateExit waits for cmd (already Start()ed under ctx) and
// translates its result into emuCmd's own exit code: 0 if it was stopped
// via Ctrl-C/SIGTERM (ctx canceled), the child's own exit code if it
// simply exited non-zero, or 1 for any other error.
func waitAndTranslateExit(cmd *exec.Cmd, ctx context.Context, stderr io.Writer) int {
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// findEmulatorDir locates a sibling emulator checkout next to the bil repo
// -- the same convention emulator/README.md's own Install section and
// every build command in this ecosystem already assumes -- by walking
// upward from the current working directory (e.g. running `bil emu` from
// inside examples/ must still find it) until some ancestor's own sibling
// "emulator" directory has a go.mod. Deliberately no env var or config
// override: one convention, consistently.
func findEmulatorDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "..", "emulator")
		if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached filesystem root without finding one
		}
		dir = parent
	}
	return "", fmt.Errorf("bil emu: no sibling emulator checkout found near %s (see emulator/README.md's Install section)", dir)
}

// goBuildWasm builds the Go package in dir (a directory containing exactly
// one main.go) as a wasm binary, matching emulator/README.md's Quickstart
// build command exactly (GOOS=js GOARCH=wasm go build).
func goBuildWasm(dir, out string) error {
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, output)
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// openInBrowser opens url in the platform's default browser -- there's no
// existing helper for this in either repo.
func openInBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
