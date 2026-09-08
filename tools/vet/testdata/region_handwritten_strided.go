// A hand-written data[i*k:(i+1)*k] slice inside a parFor body, not going
// through any helper at all — bilc's own token-level checker used to
// reject this outright ("raw-sliced... rejected") before it was removed in
// favor of this package being the sole enforcer. Exercises
// regionSelfConsistent's plain-integer-coefficient case (k is a literal
// constant, not an opaque atom like splitN's chunk). Must be accepted.
package main

import "sync"

func worker(chunk []int) {
	chunk[0] = 1
}

func main() {
	data := make([]int, 12)

	par(
		func() {
			parFor(4, func(i int) {
				worker(data[i*3 : (i+1)*3 : (i+1)*3])
			})
		},
		func() {
			println("unrelated")
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
