// Two branches both close the same channel — double close panics in Go.
// Bil's point-to-point discipline implies exactly one owner closes a
// channel; this is exactly as much a same-direction conflict as two
// branches both sending or both receiving.
package main

import "sync"

func main() {
	done := make(chan int)

	par(
		func() { close(done) },
		func() { close(done) },
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
