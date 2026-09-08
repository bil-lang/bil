// Two proc branches both send to the same channel — Bil's point-to-point
// rule forbids a channel being used for output in more than one component of a
// parallel. Mirrors violation_procs.go's two-receivers shape on the send
// side, closing a gap in the regression matrix left by dropping bilc's own
// checkParBranchChannelUsage (which had a dedicated fixture for this,
// chan-shared-output.bil, since removed).
package main

import "sync"

func sender(id int, out chan<- int) {
	out <- id
}

func receiver(in <-chan int) {
	x := <-in
	println(x)
}

func main() {
	c := make(chan int)

	par(
		func() { sender(1, c) },
		func() { sender(2, c) },
		func() { receiver(c) },
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
