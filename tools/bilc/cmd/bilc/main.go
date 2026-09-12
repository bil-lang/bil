// Command bilc is the standalone bilc CLI: bilc source.bil source.go.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bilc"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: bilc source.bil source.go")
		os.Exit(2)
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := bilc.Transform(os.Args[1], src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "transform error:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// A `placed par`-using source also gets a companion topology
	// manifest — see PlacementManifest's doc comment. Written next to the
	// .go output, not the .bil source, since that's where the emulator's
	// own regenerate-command convention (see ../../../emulator/README.md)
	// already expects generated artifacts to live; nil for the common
	// case of a program with no placement at all, so nothing is written.
	manifest, err := bilc.PlacementManifest(os.Args[1], src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifest error:", err)
		os.Exit(1)
	}
	if manifest != nil {
		manifestPath := strings.TrimSuffix(os.Args[2], filepath.Ext(os.Args[2])) + ".topology.yaml"
		if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
