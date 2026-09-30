// Command bil is the unified Bil command-line tool.
//
//	bil run [-dry-run [-rows N] [-cols N]] <file.bil>   transpile, vet, and (if clean) execute
//	bil vet <file.bil>                                  transpile and vet only
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"bilc"
	"vet"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bil run [-dry-run] [-rows N] [-cols N] <file.bil>")
	fmt.Fprintln(os.Stderr, "       bil vet <file.bil>")
	fmt.Fprintln(os.Stderr, "       bil emu [-target wasm|multicore] [-rows N] [-cols N] [-addr host:port] [-open] <file.bil>")
}

func main() {
	vet.EnsureGOROOT()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch cmd := os.Args[1]; cmd {
	case "run":
		os.Exit(runMain(os.Args[2:], os.Stdout, os.Stderr))
	case "vet":
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		os.Exit(runCmd(os.Args[2], false, false, 0, 0, os.Stdout, os.Stderr))
	case "emu":
		os.Exit(emuMain(os.Args[2:], os.Stdout, os.Stderr))
	default:
		usage()
		os.Exit(2)
	}
}

// runMain parses `bil run`'s flags and calls runCmd, matching emuMain's own
// shape (see emu.go).
func runMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// No backtick-quoted words in these usage strings -- flag.PrintDefaults
	// treats a single back-quoted substring as the flag's value-type name
	// (see emu.go's own target/rows/cols flags for the same note).
	dryRun := fs.Bool("dry-run", false, "ignore all placement directives (placed par/processor(...)/place ... at link[...])"+
		" and run the program as ordinary concurrent Go on this machine instead of refusing it -- see -rows/-cols."+
		" Grid cells are goroutines, not OS processes or threads, and GOMAXPROCS is forced to 1, so the whole"+
		" program genuinely runs on one CPU")
	rows := fs.Int("rows", 3, "-dry-run only: grid rows to resolve processor(...) clauses and link[...] wiring against")
	cols := fs.Int("cols", 3, "-dry-run only: grid cols (see -rows)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bil run [-dry-run] [-rows N] [-cols N] <file.bil>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if !*dryRun && (fs.Lookup("rows").Value.String() != "3" || fs.Lookup("cols").Value.String() != "3") {
		fmt.Fprintln(stderr, "bil run: -rows/-cols only apply with -dry-run")
		return 2
	}
	return runCmd(fs.Arg(0), true, *dryRun, *rows, *cols, stdout, stderr)
}

// runCmd reads src, transpiles it with bilc, writes the result to a temp
// file, and vets it with vet. If execute is true and vetting found no
// violations, it additionally runs the temp file via `go run`, inheriting
// stdin and wiring stdout/stderr to the given writers — the one
// unavoidable subprocess spawn, needed to hand off to the Go toolchain. It
// returns the process exit code to use.
//
// dryRun (only meaningful when execute is true — `bil run`, never `bil
// vet`) transpiles via bilc.TransformDryRun instead of
// bilc.TransformWithWarnings: a `placed par` block, if any, compiles to
// one goroutine per (r,c) cell of the rows*cols grid instead of a role-
// dispatch switch, so it runs locally instead of being refused. See
// bilc.TransformDryRun's own doc comment.
func runCmd(src string, execute, dryRun bool, rows, cols int, stdout, stderr io.Writer) int {
	in, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	var out []byte
	var warnings []string
	var roles map[string][]byte
	if dryRun {
		out, warnings, err = bilc.TransformDryRun(src, in, rows, cols)
	} else {
		out, warnings, err = bilc.TransformWithWarnings(src, in)
	}
	if err != nil {
		fmt.Fprintln(stderr, "transform error:", err)
		return 1
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}

	// A `placed par` program imports emulator/bilink -- vet.Check can now
	// resolve that (a generated go.mod + local `replace`, see below), so
	// only `bil run`'s execute path still needs to bail out here: a
	// multi-processor grid program can't sensibly `go run` on one
	// machine regardless of whether it vets clean. `bil vet` on the same
	// program now proceeds to a real check instead of stopping here.
	// -dry-run is exactly the exception to that refusal -- its
	// bilc.TransformDryRun output never imports emulator/bilink at all
	// (see TransformDryRun's own doc comment), so this check is skipped
	// entirely rather than ever seeing roles != nil.
	if !dryRun {
		roles, err = bilc.RoleBinaries(src, in)
		if err != nil {
			fmt.Fprintln(stderr, "role split error:", err)
			return 1
		}
		if roles != nil && execute {
			fmt.Fprintf(stderr, "bil: %s uses `link[...]`/`placed par` — it targets the grid emulator, not this machine. Use `bil emu`, or `bil run -dry-run`, instead of `bil run`.\n", src)
			return 1
		}
	}

	tmp, err := os.CreateTemp("", "bil-*.go")
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

	var extraGoMod []string
	if roles != nil {
		emulatorDir, err := findEmulatorDir()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		extraGoMod = []string{"require emulator v0.0.0", "replace emulator => " + emulatorDir}
	}
	messages, err := vet.Check(tmp.Name(), extraGoMod...)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(messages) > 0 {
		for _, m := range messages {
			fmt.Fprintln(stderr, m)
		}
		if execute {
			fmt.Fprintf(stderr, "\nbil: static analysis failed for %s — not running (see violation(s) above)\n", src)
		}
		return 1
	}
	if !execute {
		return 0
	}

	c := exec.Command("go", "run", tmp.Name())
	if dryRun {
		// "Ignores all placement directives (runs the code concurrently on
		// a single CPU)" -- goroutines already give the concurrency;
		// forcing GOMAXPROCS=1 makes "single CPU" literally true too,
		// rather than just "no real/emulated grid hardware".
		c.Env = append(os.Environ(), "GOMAXPROCS=1")
	}
	c.Stdin = os.Stdin
	c.Stdout = stdout
	c.Stderr = stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
