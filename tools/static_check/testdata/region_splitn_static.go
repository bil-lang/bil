// Matches 11-recursive-proc.bil's shape: static par branches, splitN
// indexed by distinct integer literals — but with explicit writes (the
// real example only reads), so the disjointness proof is actually
// exercised, not trivially satisfied by "nothing writes anything".
// Must NOT be flagged.
package main

import "sync"

func mutate(data []int) {
	data[0] = 99
}

func main() {
	data := make([]int, 8)
	chunks := splitN(data, 2)

	par(
		func() { mutate(chunks[0]) },
		func() { mutate(chunks[1]) },
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
