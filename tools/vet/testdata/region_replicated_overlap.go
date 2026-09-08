// A replicated construct with a genuinely overlapping hand-written stride
// (i*2 : i*2+3 — width 3 but stride only 2, so consecutive instantiations
// overlap by one element) — the shape no existing bilc check inspects at
// all (a replicated par body). Must be flagged.
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
				worker(data[i*2 : i*2+3])
			})
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
