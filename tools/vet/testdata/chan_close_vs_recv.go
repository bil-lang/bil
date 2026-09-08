// One branch closes a channel while another only receives from it — this
// is the standard "close signals completion" idiom (receive from a closed
// channel is well-defined: zero value, ok == false) and must NOT be
// flagged, unlike close vs send.
package main

import "sync"

func main() {
	done := make(chan int)

	par(
		func() { close(done) },
		func() { <-done },
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
