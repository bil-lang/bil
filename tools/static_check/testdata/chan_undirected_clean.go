// One sender proc, one receiver proc, sharing one channel through an
// *undirected* `chan int` parameter (not `chan<-`/`<-chan`) — still the
// legal one-input/one-output shape, and must NOT be flagged. bilc's own
// checkParBranchChannelUsage used to reject this outright, purely because
// the parameter type wasn't directional, regardless of actual usage — a
// false positive fixed by dropping that check in favor of this one, which
// resolves real send/receive syntax instead of trusting a declared type
// (chan-shared-undirected.bil, moved from tools/bilc/testdata/err to
// tools/bilc/testdata/ok).
package main

import "sync"

func sender(c chan int) {
	c <- 42
	c <- 99
}

func receiver(c chan int) {
	x := <-c
	y := <-c
	println(x)
	println(y)
}

func main() {
	c := make(chan int)

	par(
		func() { sender(c) },
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
