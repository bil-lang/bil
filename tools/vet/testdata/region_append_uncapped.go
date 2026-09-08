// append on a tracked region whose capacity isn't bounded to its own
// length (a plain two-index slice) — the capacity gap: chunk's logical
// bounds don't overlap anything, but its capacity reaches past its own
// end into the rest of data's backing array, so append could silently
// overwrite a neighbor without ever reallocating. Must be flagged
// regardless of what (if anything) the disjointness proof would say.
package main

import "sync"

func grow(s []int) {
	s = append(s, 99)
	_ = s
}

func main() {
	data := make([]int, 10)
	chunk := data[0:5] // no third index — capacity reaches to len(data), not 5

	par(
		func() { grow(chunk) },
		func() { println("unrelated") },
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
