// A package-level variable mutated by a proc called from one branch, read
// by a proc called from another — neither takes it as a parameter at all,
// so no substitution is involved; the only way to see this is to recurse
// into both calls and notice they both touch the same package-level free
// variable. Must be flagged.
package main

import "sync"

var counter int

func bump() {
	counter++
}

func read() {
	println(counter)
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
