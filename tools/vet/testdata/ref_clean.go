// A pointer used entirely within one branch — no sharing, nothing to
// check. Must NOT be flagged.
package main

import "sync"

func bump(p *int) {
	*p = *p + 1
}

func main() {
	x := 0
	p := &x

	par(
		func() { bump(p) },
		func() { println("unrelated") },
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
