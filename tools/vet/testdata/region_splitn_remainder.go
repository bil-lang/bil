// Same shape as region_splitn_replicated.go, but data's length (10) isn't
// evenly divisible by the worker count (3) — chunk=3, remainder 1 folded
// into the last chunk (splitN's own `if i==n-1 { hi = len(s) }`). Neither
// 11-recursive-proc.bil nor 12-scatter-gather.bil exercises this (both
// divide evenly), so this is the only thing that proves the piecewise
// last-iteration handling actually works, not just that it's inert. Must
// NOT be flagged.
package main

import "sync"

func worker(chunk []int) {
	chunk[0] = 1
}

func main() {
	data := make([]int, 10)
	chunks := splitN(data, 3)

	par(
		func() {
			parFor(3, func(i int) {
				worker(chunks[i])
			})
		},
		func() {
			println("unrelated")
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
