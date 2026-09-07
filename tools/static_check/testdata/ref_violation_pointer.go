// An existing pointer variable — not a fresh &x — shared into two
// branches' proc calls. Neither branch does a literal &x, so bilc's own
// checkNoPointerParams (which only fires on that exact shape) would never
// catch this; variables.go excludes pointer types entirely. This is the
// gap CheckRefSharing exists to close. Must be flagged regardless of
// what either branch actually does with p — mere sharing is the
// violation.
package main

import "sync"

func mutate(q *int) {
	*q = 5
}

func read(q *int) {
	println(*q)
}

func main() {
	x := 0
	p := &x

	par(
		func() { mutate(p) },
		func() { read(p) },
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
