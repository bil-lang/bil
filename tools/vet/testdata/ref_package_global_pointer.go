// A package-level pointer touched only inside two different same-file
// callees, neither receiving it as a parameter at all — proves the
// interprocedural-reach piece (reused from variables.go/channels.go's
// established pattern) works for this rule too, with no substitution
// needed: merely referencing the same free identity from two branches'
// reach is itself the violation. Must be flagged.
package main

import "sync"

var shared = new(int)

func bump() {
	*shared = *shared + 1
}

func read() {
	println(*shared)
}

func main() {
	par(
		func() { bump() },
		func() { read() },
	)
}

func par(branches ...func()) {
	var wg sync.WaitGroup
	wg.Add(len(branches))
	for _, b := range branches {
		go func(b func()) {
			defer wg.Done()
			b()
		}(b)
	}
	wg.Wait()
}
