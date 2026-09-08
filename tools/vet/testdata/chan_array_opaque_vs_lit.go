// Two branches both send, one via a literal index, one via an arbitrary
// computed index — can't prove they reach different channels, so this
// conservatively conflicts (the same "can't verify, so reject" stance
// as everywhere else in this project), unlike the distinct-literal case.
// Must be flagged.
package main

import "sync"

func pick() int { return 1 }

func main() {
	chans := makeChans[int](2)
	k := pick()

	par(
		func() { chans[0] <- 1 },
		func() { chans[k] <- 2 },
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
