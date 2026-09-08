// The same region read (never written) in two branches — Bil's rule
// permits any number of readers so long as nothing assigns. Must NOT be
// flagged.
package main

import "sync"

func read(s []int) {
	println(s[0])
}

func main() {
	data := make([]int, 10)

	par(
		func() { read(data) },
		func() { read(data) },
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
