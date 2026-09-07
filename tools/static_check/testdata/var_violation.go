// A bare-block branch writes a free int, another bare-block branch reads
// it — the exact shape none of bilc's own checks (call-shaped branches
// only) can see. Must be flagged.
package main

import "sync"

func main() {
	x := 0

	par(
		func() {
			x = 1
		},
		func() {
			println(x)
		},
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
