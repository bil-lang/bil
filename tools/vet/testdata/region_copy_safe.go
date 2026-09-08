// Same shape as region_copy_violation.go, but the two copy targets are
// genuinely disjoint. Must NOT be flagged.
package main

import "sync"

func fill(dst []int, src []int) {
	copy(dst, src)
}

func main() {
	data := make([]int, 10)
	src := make([]int, 5)

	par(
		func() { fill(data[0:5], src) },
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
