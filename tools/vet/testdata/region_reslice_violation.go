// Reslicing within capacity (`view = view[:6]`), not append — the same
// self-referential-reassignment tolerance, exercised through a genuinely
// overlapping pair of windows to confirm tracing survives a reslice
// reassignment, not just that it isn't lost on a safe case. Must be
// flagged.
package main

import "sync"

func writeLeft(data []int) {
	view := data[0:6]
	view = view[:6] // self-referential reslice, must not lose tracing
	view[0] = 1
}

func writeRight(data []int) {
	view := data[5:10]
	view = view[:5]
	view[0] = 2
}

func main() {
	data := make([]int, 10)

	par(
		func() { writeLeft(data) },
		func() { writeRight(data) },
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
