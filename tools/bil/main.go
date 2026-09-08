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
}

func main() {
	if len(os.Args) != 3 {
		usage()
		os.Exit(2)
	}
	cmd, src := os.Args[1], os.Args[2]
	switch cmd {
	case "run":
		os.Exit(runCmd(src, true, os.Stdout, os.Stderr))
	case "vet":
		os.Exit(runCmd(src, false, os.Stdout, os.Stderr))
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
	out, err := bilc.Transform(in)
	if err != nil {
		fmt.Fprintln(stderr, "transform error:", err)
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

	messages, err := vet.Check(tmp.Name())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(messages) > 0 {
		for _, m := range messages {
			fmt.Fprintln(stderr, m)
		}
		if execute {
			fmt.Fprintf(stderr, "\nbil: static analysis failed for %s — not running (see violation(s) above; positions refer to the transpiled Go, not the .bil source)\n", src)
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
