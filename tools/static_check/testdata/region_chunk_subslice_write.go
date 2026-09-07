// Outer array split into chunks via a replicated par, then a further
// slice taken *within* a chunk, written into directly (no append). Since
// splitN's chunks are capacity-capped (s[lo:hi:hi]), Go's own runtime
// bounds check guarantees this narrower sub-window can never exceed the
// chunk's own disjoint extent — so it inherits the chunk's already-proven
// self-consistency rather than needing its own narrower bounds proven
// numerically (which, for a symbolic stride, generally can't be). Must
// NOT be flagged — this used to be a false rejection.
package main

import "sync"

func worker(chunk []int) {
	sub := chunk[1:3]
	sub[0] = 99
}

func main() {
	data := make([]int, 12)
	chunks := splitN(data, 4)

	par(
		func() {
			parFor(4, func(i int) {
				worker(chunks[i])
			})
		},
	)
}

func splitN[T any](s []T, n int) [][]T {
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

func parFor(n int, body func(int)) {
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			body(i)
		}(i)
	}
	wg.Wait()
}
