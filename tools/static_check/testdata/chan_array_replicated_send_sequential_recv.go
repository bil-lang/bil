// The real 12-scatter-gather.bil shape: a replicated parFor sends via
// chans[i] (i is parFor's own bound var), a sequential for-range loop in
// a sibling branch receives via chans[j] (j is a *different* induction
// variable, from an unrelated loop). This is the core capability this
// check must not regress on — channel-array elements were previously
// completely invisible to CheckChannelUsage, in every par shape. Must
// NOT be flagged.
package main

import "sync"

func worker(i int, out chan<- int) {
	out <- i
}

func main() {
	n := 4
	chans := makeChans[int](n)

	par(
		func() {
			parFor(n, func(i int) {
				worker(i, chans[i])
			})
		},
		func() {
			total := 0
			for j := range n {
				v := <-chans[j]
				total += v
			}
			println(total)
		},
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

func parFor(n int, body func(int)) {
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			body(i)
		}(i)
	}
	wg.Wait()
}
