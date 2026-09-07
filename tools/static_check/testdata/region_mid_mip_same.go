// Two independently-declared variables (mid, mip), each := len(data)/2 —
// the exact historical concern splitN's own doc comment cites (a token-
// based checker couldn't trace them back to their definitions and reject
// only when they genuinely drift). Real value-tracing proves them equal
// because they resolve to the identical normalized form, not because they
// share a name. Must NOT be flagged.
package main

import "sync"

func writeLeft(s []int) {
	s[0] = 1
}

func writeRight(s []int) {
	s[0] = 2
}

func main() {
	data := make([]int, 10)
	mid := len(data) / 2

	par(
		func() { writeLeft(data[0:mid]) },
		func() {
			mip := len(data) / 2
			writeRight(data[mip:len(data)])
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
