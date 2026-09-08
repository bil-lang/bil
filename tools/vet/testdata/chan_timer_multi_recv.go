// A named timer channel received in more than one par branch — Bil's
// own explicit exemption from the point-to-point channel rule ("a timer
// may be used for input by any number of
// components of a parallel"). Same shape as violation_bare.go (two
// branches both receive from the same channel), but the channel is a
// timer, not an ordinary one. Must NOT be flagged.
package main

import (
	"sync"
	"time"
)

func main() {
	tick := time.After(time.Second)

	par(
		func() { <-tick },
		func() { <-tick },
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
