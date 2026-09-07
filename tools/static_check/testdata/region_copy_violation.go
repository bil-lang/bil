// copy(dst, src) writes into dst — a hazard with no reassignment, no
// append, and no index expression at all, so none of the other write
// detection paths would ever see it without dedicated handling. Two
// branches copy into overlapping windows of the same array. Must be
// flagged.
package main

import "sync"

func fill(dst []int, src []int) {
	copy(dst, src)
}

func main() {
	data := make([]int, 10)
	src := make([]int, 6)

	par(
		func() { fill(data[0:6], src) },
		func() { fill(data[5:10], src) },
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
