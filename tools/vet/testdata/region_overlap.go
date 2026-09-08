// Two branches write to plainly overlapping hand-written bounds
// (data[0:6] vs data[5:10] — index 5 is in both). Must be flagged.
package main

import "sync"

func write(s []int) {
	s[0] = 1
}

func main() {
	data := make([]int, 10)

	par(
		func() { write(data[0:6]) },
		func() { write(data[5:10]) },
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
