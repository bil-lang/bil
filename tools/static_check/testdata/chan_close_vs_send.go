// One branch closes a channel while another sends to it — a send on a
// closed channel panics in Go, an asymmetric hazard conflictsAmong's
// same-direction grouping can't express on its own.
package main

import "sync"

func main() {
	done := make(chan int)

	par(
		func() { close(done) },
		func() { done <- 1 },
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
