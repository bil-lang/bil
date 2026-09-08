// Command bilc is the standalone bilc CLI: bilc source.bil source.go.
package main

import (
	"fmt"
	"os"

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
	out, err := bilc.Transform(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "transform error:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
