// Same self-append-reassignment shape as region_selfappend_uncapped.go,
// but the sub-slice itself uses a three-index expression, so its capacity
// is bounded to its own length — append is then guaranteed to reallocate
// rather than alias. Confirms isSelfAppendReassign's tracing fix doesn't
// over-trigger once the slice is actually safe. Must NOT be flagged.
package main

import "sync"

func worker(chunk []int) {
	sub := chunk[1:3:3]
	sub = append(sub, 99)
	_ = sub
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
