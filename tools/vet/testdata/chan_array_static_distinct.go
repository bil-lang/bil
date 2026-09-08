// Static branches, chans[0]/chans[1] literal indices, one send one recv —
// distinct compile-time-constant identities. Must NOT be flagged.
package main

import "sync"

func main() {
	chans := makeChans[int](2)

	par(
		func() { chans[0] <- 1 },
		func() { <-chans[1] },
	)
}

func makeChans[T any](n int) []chan T {
	cs := make([]chan T, n)
	for i := range cs {
		cs[i] = make(chan T)
	}
	return cs
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
