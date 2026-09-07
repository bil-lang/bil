// A channel-array element used directly — as a call argument, and as the
// direct operand of a send/receive — never aliased into another variable
// or returned. This stays fully visible to channels.go's own
// direction-conflict reasoning and isn't a confinement leak. Must NOT be
// flagged.
package main

func worker(out chan<- int) {
	out <- 1
}

func main() {
	chans := makeChans[int](2)
	worker(chans[0])
	<-chans[1]
}

func makeChans[T any](n int) []chan T {
	cs := make([]chan T, n)
	for i := range cs {
		cs[i] = make(chan T)
	}
	return cs
}
