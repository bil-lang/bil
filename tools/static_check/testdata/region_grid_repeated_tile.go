// The same literal (i,j) tile reused by two branches — each splitN2D tile
// is meant for exactly one consumer, so a repeated pair is almost always a
// copy-paste mistake, not a deliberate choice — mirrors bilc's own
// existing "index reused" rule for the 1D case. Must be flagged.
package main

import "sync"

func mutate(tile [][]int) {
	tile[0][0] = 99
}

func main() {
	matrix := make([][]int, 4)
	for i := range matrix {
		matrix[i] = make([]int, 4)
	}
	tiles := splitN2D(matrix, 2, 2)

	par(
		func() { mutate(tiles[0][0]) },
		func() { mutate(tiles[0][0]) },
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
