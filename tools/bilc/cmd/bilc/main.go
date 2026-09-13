// Command bilc is the standalone bilc CLI: bilc source.bil source.go.
package main

import (
	"fmt"
	"os"
	"path/filepath"

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

	// Per-role standalone binaries and the host-facing deploy manifest --
	// see this feature's plan (link-native boot cascade): a placed-par
	// program also gets one complete, standalone Go source per distinct
	// role under roles/<name>/main.go, plus roles/deploy.json describing
	// every reachable leaf's clause match, if/else condition chain, and
	// link-index binds for a host to resolve concrete role assignment at
	// grid-launch time. Both are nil/empty for a file with no placed par
	// at all.
	roles, err := bilc.RoleBinaries(os.Args[1], src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "role binaries error:", err)
		os.Exit(1)
	}
	if len(roles) > 0 {
		outDir := filepath.Dir(os.Args[2])
		for name, code := range roles {
			roleDir := filepath.Join(outDir, "roles", name)
			if err := os.MkdirAll(roleDir, 0o755); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if err := os.WriteFile(filepath.Join(roleDir, "main.go"), code, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}

		deploy, err := bilc.DeployManifest(os.Args[1], src)
		if err != nil {
			fmt.Fprintln(os.Stderr, "deploy manifest error:", err)
			os.Exit(1)
		}
		if deploy != nil {
			if err := os.WriteFile(filepath.Join(outDir, "roles", "deploy.json"), deploy, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
	}
}
