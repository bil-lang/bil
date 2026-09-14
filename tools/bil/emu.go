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
	rows := fs.Int("rows", 0, "grid rows (0 = auto: 1 for a plain chan/proc/par program,"+
		" or the smallest grid that gives every placed processor its own node"+
		" for a `placed par` program -- see -cols)")
	cols := fs.Int("cols", 0, "grid cols (0 = auto, see -rows). NOTE: forcing a `placed par`"+
		" program onto too small a grid can silently under-exercise it -- e.g."+
		" processor(0,0) and processor(0,cols-1) collide into the same generated"+
		" Go switch case when cols=1, and only the first match ever fires")
	addr := fs.String("addr", "localhost:8787", "address for the emulator's static file server to listen on")
	open := fs.Bool("open", true, "open the default browser once the server is ready")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bil emu [-rows N] [-cols N] [-addr host:port] [-open] <file.bil>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	return emuCmd(fs.Arg(0), *rows, *cols, *addr, *open, stdout, stderr)
}

// emuCmd transpiles src with bilc, vets it (refusing to proceed on any
// violation, exactly like runCmd's execute=true path), detects placement
// via bilc.RoleBinaries/DeployManifest, and runs the result inside the
// browser/WASM grid emulator at a sibling ../emulator checkout instead of
// natively -- see emulator/README.md's Install section for that
// convention, which this reuses rather than inventing an env var or
// config override.
func emuCmd(src string, rows, cols int, addr string, openBrowser bool, stdout, stderr io.Writer) int {
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

	// vet.Check can only ever run on a program with no link[...]/placed
	// par usage: its importer (go/importer's "source" mode, see
	// tools/vet/check.go) does classic GOPATH-style resolution with no
	// real Go-modules awareness, so it can never resolve
	// "emulator/nodeprog/bilink" -- not a location problem, moving the
	// file doesn't help. This is the exact same, already-accepted
	// limitation `bil vet`/`bil run` have always had on these programs
	// (confirmed directly: `bil vet` on a link/placed-par example fails
	// identically) -- the emulator's own `go build` step (module-aware,
	// unlike vet's importer) is what actually verifies these programs,
	// same as it always has.
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

	scratchDir, err := os.MkdirTemp(filepath.Join(emulatorDir, "nodeprog"), "bilemu-*")
	if err != nil {
		fmt.Fprintln(stderr, "creating build scratch dir:", err)
		return 1
	}
	defer os.RemoveAll(scratchDir)

	serveDir, err := os.MkdirTemp("", "bil-emu-serve-*")
	if err != nil {
		fmt.Fprintln(stderr, "creating serve dir:", err)
		return 1
	}
	defer os.RemoveAll(serveDir)

	for _, name := range []string{"index.html", "node-worker.js", "wasm_exec.js"} {
		if err := copyFile(filepath.Join(emulatorDir, "static", name), filepath.Join(serveDir, name)); err != nil {
			fmt.Fprintln(stderr, "copying static assets:", err)
			return 1
		}
	}

	if roles == nil {
		// No placement -- one binary, no roles/deploy.json at all. index.html's
		// own startGrid falls back to this shape whenever roles/deploy.json
		// 404s, so nothing else needs to know which path ran.
		if rows == 0 {
			rows = 1
		}
		if cols == 0 {
			cols = 1
		}
		if err := os.WriteFile(filepath.Join(scratchDir, "main.go"), out, 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := goBuildWasm(scratchDir, filepath.Join(serveDir, "node.wasm")); err != nil {
			fmt.Fprintln(stderr, "building node.wasm:", err)
			return 1
		}
	} else {
		manifest, err := bilc.DeployManifest(src, in)
		if err != nil {
			fmt.Fprintln(stderr, "deploy manifest error:", err)
			return 1
		}
		if rows == 0 || cols == 0 {
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
		rolesDir := filepath.Join(serveDir, "roles")
		if err := os.MkdirAll(rolesDir, 0o755); err != nil {
			fmt.Fprintln(stderr, err)
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
			if err := goBuildWasm(roleScratch, filepath.Join(rolesDir, role+".wasm")); err != nil {
				fmt.Fprintln(stderr, fmt.Sprintf("building role %q: %v", role, err))
				return 1
			}
		}
		if err := os.WriteFile(filepath.Join(rolesDir, "deploy.json"), manifest, 0o644); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}

	// Build cmd/serve once and exec the binary directly, rather than
	// `go run ./cmd/serve`: go run spawns the actual compiled binary as
	// its own child process and does not forward a kill signal to it --
	// confirmed directly (SIGINT-ing `go run` leaves the real serve
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
	buildServe := exec.Command("go", "build", "-o", serveBin, "./cmd/serve")
	buildServe.Dir = emulatorDir
	if output, err := buildServe.CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "building emulator's cmd/serve: %v\n%s", err, output)
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

	if err := serveCmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return 0 // stopped via Ctrl-C/SIGTERM, not a real failure
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

// findEmulatorDir locates a sibling ../emulator checkout relative to the
// current working directory -- the same convention emulator/README.md's
// own Install section and every build command in this ecosystem already
// assumes. Deliberately no env var or config override: one convention,
// consistently.
func findEmulatorDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cwd, "..", "emulator")
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("bil emu: no sibling emulator checkout found at %s (see emulator/README.md's Install section)", dir)
	}
	return dir, nil
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
