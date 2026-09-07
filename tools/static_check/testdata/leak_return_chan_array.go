// A channel-array element returned directly from a function — the same
// hazard as `return c` for a scalar channel. Must be flagged (LeakReturn).
package main

func pick(chans []chan int) chan int {
	return chans[0]
}

func main() {
	chans := makeChans[int](2)
	_ = pick(chans)
}

func makeChans[T any](n int) []chan T {
	cs := make([]chan T, n)
	for i := range cs {
		cs[i] = make(chan T)
	}
	return cs
}
