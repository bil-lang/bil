// Matches 12-scatter-gather.bil's shape: splitN inside a replicated
// parFor body, indexed by the replication variable — but with an
// explicit write (the real example only reads), so the self-consistency
// proof (regionSelfConsistent) is actually exercised. This is the shape
// no existing bilc check inspects at all — trusted purely by construction
// (splitN's contract + parFor's "every i in [0,n) exactly once"), never
// actually verified before this file. Must NOT be flagged.
package main

import "sync"

func worker(chunk []int) {
	chunk[0] = 1
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
