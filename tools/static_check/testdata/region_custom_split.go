// A helper shaped exactly like splitN but *not* named it — proves the
// summarizer recognizes the general shape (make + range-loop populate +
// return), not the name "splitN" specifically. Must NOT be flagged, same
// as region_splitn_static.go.
package main

import "sync"

func mutate(data []int) {
	data[0] = 99
}

func main() {
	data := make([]int, 8)
	chunks := splitInto(data, 2)

	par(
		func() { mutate(chunks[0]) },
		func() { mutate(chunks[1]) },
	)
}

func splitInto[T any](s []T, n int) [][]T {
	chunk := len(s) / n
	out := make([][]T, n)
	for i := range n {
		lo := i * chunk
		hi := lo + chunk
		if i == n-1 {
			hi = len(s)
		}
		out[i] = s[lo:hi:hi]
	}
	return out
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
