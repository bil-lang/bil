// Two branches both send to the same literal index — the case bilc's own
// checkParBranchChannelUsage already catches for static branches via
// source-text identity; now proven by static_check too, and via a real
// channel-array element, not just a base-identifier heuristic. Must be
// flagged.
package main

import "sync"

func main() {
	chans := makeChans[int](2)

	par(
		func() { chans[0] <- 1 },
		func() { chans[0] <- 2 },
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
