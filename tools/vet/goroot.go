package vet

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// EnsureGOROOT re-execs the current process with a corrected GOROOT
// environment variable when needed. Check's type-checking (via
// go/importer's "source" mode) resolves imports through go/build, which —
// even for standard-library packages — shells out to the `go` binary
// located at runtime.GOROOT()/bin/go. runtime.GOROOT() falls back to the
// GOROOT of the machine that built this binary when the GOROOT
// environment variable isn't set, which for a prebuilt, distributed `bil`
// binary is essentially never the machine it's running on — so that path
// doesn't exist there, and every Check call fails. Since that fallback
// value is baked into the process at package-init time (before main
// runs), it can't be fixed in-place; re-executing a fresh child process
// with GOROOT set correctly is the only way to fix it for this run.
//
// Safe to call unconditionally from a main package before anything else
// runs: it's a no-op whenever the embedded GOROOT already resolves (e.g.
// a binary built and run on the same machine), and it gives up quietly
// (leaving Check to fail with its own, clearer "go" error) if no working
// `go` can be found on PATH at all.
func EnsureGOROOT() {
	if os.Getenv("BIL_GOROOT_FIXED") != "" {
		return
	}
	if fi, err := os.Stat(filepath.Join(runtime.GOROOT(), "bin", "go")); err == nil && !fi.IsDir() {
		return
	}

	realGo, err := exec.LookPath("go")
	if err != nil {
		return
	}
	out, err := exec.Command(realGo, "env", "GOROOT").Output()
	if err != nil {
		return
	}
	goroot := strings.TrimSpace(string(out))
	if goroot == "" {
		return
	}

	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(), "GOROOT="+goroot, "BIL_GOROOT_FIXED=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	code := 0
	if err := cmd.Run(); err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	os.Exit(code)
}
