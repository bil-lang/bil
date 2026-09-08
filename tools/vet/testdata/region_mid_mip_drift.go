// Same shape as region_mid_mip_same.go, but mip's own definition no
// longer matches mid's (a "+1" that was never mirrored back — the actual
// drift the historical concern was about). The proof correctly stops
// trusting them the instant their definitions diverge, rather than ever
// having assumed they were related in the first place. Must be flagged.
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
			mip := len(data)/2 - 1
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
