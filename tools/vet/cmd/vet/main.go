// Command vet is the standalone vet CLI: vet <file.go>.
package main

import (
	"fmt"
	"os"

	"vet"
)

func main() {
	vet.EnsureGOROOT()

	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: vet <file.go>")
		os.Exit(2)
	}
	messages, err := vet.Check(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, m := range messages {
		fmt.Fprintln(os.Stderr, m)
	}
	if len(messages) > 0 {
		os.Exit(1)
	}
}
