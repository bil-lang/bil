// A map shared into two branches — maps had no coverage at all before
// CheckRefSharing (not even the partial coverage pointers got from
// checkNoPointerParams). Must be flagged.
package main

import "sync"

func write(m map[string]int) {
	m["a"] = 1
}

func read(m map[string]int) {
	println(m["a"])
}

func main() {
	m := make(map[string]int)

	par(
		func() { write(m) },
		func() { read(m) },
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
