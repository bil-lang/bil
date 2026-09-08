// Two proc branches both receive from the same channel — Bil's point-to-point
// rule forbids a channel being used for input in more than one component of a
// parallel. Exercises the interprocedural (call-shaped branch) path: the
// violation is only visible by looking inside worker's body, resolved
// through its channel parameter back to the shared identity at the call
// site — exactly what bilc's own checkParBranchChannelUsage would trust
// blindly from worker's declared <-chan direction instead of verifying.
package main

import "sync"

func worker(in <-chan int) {
	x := <-in
	println(x)
}

func main() {
	in := make(chan int)

	par(
		func() { worker(in) },
		func() { worker(in) },
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
