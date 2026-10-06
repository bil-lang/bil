package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"bilc"
	"vet"
)

// runNoneTarget is `bil emu -target none`: transpile src with
// bilc.TransformDryRun, vet it, and run it via `go run` on this machine
// with GOMAXPROCS=1 -- every placed processor becomes a goroutine in one
// process, no emulator checkout or grid hardware involved. rows/cols of 0
// mean auto, same as the other targets: the smallest grid that gives every
// placed processor its own node, or 1x1 for a program with no placement.
func runNoneTarget(src string, in []byte, rows, cols int, stdout, stderr io.Writer) int {
	if rows == 0 || cols == 0 {
		manifest, err := bilc.PlacementManifest(src, in)
		if err != nil {
			fmt.Fprintln(stderr, "placement manifest error:", err)
			return 1
		}
		if manifest == nil {
			rows, cols = 1, 1
		} else {
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

	out, warnings, err := bilc.TransformDryRun(src, in, rows, cols)
	if err != nil {
		fmt.Fprintln(stderr, "transform error:", err)
		return 1
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}

	tmp, err := os.CreateTemp("", "bil-none-*.go")
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

	c := exec.Command("go", "run", tmp.Name())
	// Goroutines already give the concurrency; GOMAXPROCS=1 makes "one CPU"
	// literally true rather than just "no grid hardware".
	c.Env = append(os.Environ(), "GOMAXPROCS=1")
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
