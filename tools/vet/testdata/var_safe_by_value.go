// The same free int passed by value into two different proc calls in two
// different branches — one of which mutates its own parameter internally.
// Go copies the argument at the call boundary, so mutate's reassignment of
// its own local n can never reach main's x: this is the exact case the
// no-interprocedural-recursion design in variables.go rests on. Must NOT be
// flagged.
package main

import "sync"

func mutate(n int) {
	n = n * 2 // only ever touches mutate's own copy
	println(n)
}

func read(n int) {
	println(n)
}

func main() {
	x := 21

	par(
		func() { mutate(x) },
		func() { read(x) },
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
