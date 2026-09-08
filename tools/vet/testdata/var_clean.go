// A free int read (never assigned) in two branches — Bil's rule
// explicitly permits this ("may appear in expressions in any number of
// components... so long as it is not assigned"). Must NOT be flagged.
package main

import "sync"

func main() {
	x := 42

	par(
		func() { println(x) },
		func() { println(x * 2) },
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
