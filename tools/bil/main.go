// Command bil is the unified Bil command-line tool.
//
//	bil run <file.bil>   transpile, vet, and (if clean) execute
//	bil vet <file.bil>   transpile and vet only
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"bilc"
	"vet"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bil run <file.bil>")
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
	case "run", "vet":
		if len(os.Args) != 3 {
			usage()
			os.Exit(2)
		}
		os.Exit(runCmd(os.Args[2], cmd == "run", os.Stdout, os.Stderr))
	case "emu":
		os.Exit(emuMain(os.Args[2:], os.Stdout, os.Stderr))
	default:
		usage()
		os.Exit(2)
	}
}

// runCmd reads src, transpiles it with bilc, writes the result to a temp
// file, and vets it with vet. If execute is true and vetting found no
// violations, it additionally runs the temp file via `go run`, inheriting
// stdin and wiring stdout/stderr to the given writers — the one
// unavoidable subprocess spawn, needed to hand off to the Go toolchain. It
// returns the process exit code to use.
func runCmd(src string, execute bool, stdout, stderr io.Writer) int {
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

	// A `placed par` program imports emulator/bilink -- vet.Check can now
	// resolve that (a generated go.mod + local `replace`, see below), so
	// only `bil run`'s execute path still needs to bail out here: a
	// multi-processor grid program can't sensibly `go run` on one
	// machine regardless of whether it vets clean. `bil vet` on the same
	// program now proceeds to a real check instead of stopping here.
	roles, err := bilc.RoleBinaries(src, in)
	if err != nil {
		fmt.Fprintln(stderr, "role split error:", err)
		return 1
	}
	if roles != nil && execute {
		fmt.Fprintf(stderr, "bil: %s uses `link[...]`/`placed par` — it targets the grid emulator, not this machine. Use `bil emu` instead of `bil run`.\n", src)
		return 1
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
