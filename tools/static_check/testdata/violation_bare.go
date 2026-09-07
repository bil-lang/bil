// Same violation as violation_procs.go (two branches both receive from the
// same channel), but written directly in bare-block branches with no proc
// call at all — the shape bilc's own checkParBranchChannelUsage can't see
// into, since it only inspects call-shaped branches.
package main

import "sync"

func main() {
	in := make(chan int)

	par(
		func() {
			x := <-in
			println(x)
		},
		func() {
			y := <-in
			println(y)
		},
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
