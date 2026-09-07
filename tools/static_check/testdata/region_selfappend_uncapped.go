// Sub-slice a chunk, then reassign through append (`sub = append(sub,
// x)`) — the idiomatic Go way to grow a slice, and the shape that was
// invisible to both the disjointness proof and the append-safety check
// before isSelfAppendReassign: an ordinary reassignment disqualifies a
// variable from tracing (findSingleDef), so sub fell back to being
// treated as its own unrelated identity — local, so it slid past the
// "is this actually shared" filter too. Must be flagged.
package main

import "sync"

func worker(chunk []int) {
	sub := chunk[1:3]
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
