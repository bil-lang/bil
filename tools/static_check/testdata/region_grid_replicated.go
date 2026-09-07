// A nested replicated par (par i := range nr { par j := range nc
// {...} }) over a splitN2D-tiled matrix — the core new capability: no
// existing check inspects a nested replicated par body at all, trusted
// purely by construction until now. Must NOT be flagged.
package main

import "sync"

func worker(tile [][]int) {
	tile[0][0] = 1
}

func main() {
	matrix := make([][]int, 4)
	for i := range matrix {
		matrix[i] = make([]int, 8)
	}
	tiles := splitN2D(matrix, 2, 4)

	par(
		func() {
			parFor(2, func(i int) {
				parFor(4, func(j int) {
					worker(tiles[i][j])
				})
			})
		},
	)
}

func splitN2D[T any](m [][]T, nr, nc int) [][][][]T {
	rchunk := len(m) / nr
	cchunk := len(m[0]) / nc
	out := make([][][][]T, nr)
	for i := range nr {
		r0 := i * rchunk
		r1 := r0 + rchunk
		if i == nr-1 {
			r1 = len(m)
		}
		out[i] = make([][][]T, nc)
		for j := range nc {
			c0 := j * cchunk
			c1 := c0 + cchunk
			if j == nc-1 {
				c1 = len(m[0])
			}
			tile := make([][]T, r1-r0)
			for r := r0; r < r1; r++ {
				tile[r-r0] = m[r][c0:c1:c1]
			}
			out[i][j] = tile
		}
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
