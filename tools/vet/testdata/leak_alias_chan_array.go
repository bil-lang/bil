// A channel-array element aliased into a local variable — the same
// hazard as `x := c` for a scalar channel, since channels.go's own
// direction-conflict reasoning has no way to trace x back to chans[0].
// Must be flagged (LeakAlias).
package main

func main() {
	chans := makeChans[int](2)
	x := chans[0]
	x <- 1
}

func makeChans[T any](n int) []chan T {
	cs := make([]chan T, n)
	for i := range cs {
		cs[i] = make(chan T)
	}
	return cs
}
