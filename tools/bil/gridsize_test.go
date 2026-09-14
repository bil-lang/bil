package main

import "testing"

func TestInferGridSizeNoPlacement(t *testing.T) {
	rows, cols, err := inferGridSize([]byte(`{"leaves":[{"match":{"default":true},"proc":"idle"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 || cols != 1 {
		t.Errorf("got %dx%d, want 1x1 for an all-default manifest", rows, cols)
	}
}

// TestInferGridSizePlacedController mirrors examples/20-placed-controller.bil's
// actual deploy.json shape: controller at (0,0), rowEnd at (0,cols-1), relay
// wildcarding the rest of row 0, idle wildcarding everything else. The
// 6x7 default is collision-free (controller and rowEnd land on distinct
// cells at cols=7), so it should be preferred over any smaller size.
func TestInferGridSizePlacedController(t *testing.T) {
	manifest := []byte(`{
		"leaves": [
			{"match": {"row": "0", "col": "0"}, "proc": "controller"},
			{"match": {"row": "0", "col": "cols-1"}, "proc": "rowEnd"},
			{"match": {"row": "0", "col": "*"}, "proc": "relay"},
			{"match": {"default": true}, "proc": "idle"}
		]
	}`)
	rows, cols, err := inferGridSize(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 6 || cols != 7 {
		t.Errorf("got %dx%d, want the default 6x7 (already collision-free for this manifest)", rows, cols)
	}
}

// TestInferGridSizeMeshRipple is a regression test for a real bug: both
// origin (col 0) and reflect (col cols-1) wildcard *row* -- an earlier
// version of inferGridSize skipped any leaf with a wildcard on *either*
// axis entirely, so it never compared origin against reflect at all and
// happily returned 1x1, where col 0 == col cols-1 and reflect could
// never fire. Two leaves sharing the same wildcard pattern (both
// wildcard row here) must still be distinguished on their shared
// concrete axis (col).
func TestInferGridSizeMeshRipple(t *testing.T) {
	manifest := []byte(`{
		"leaves": [
			{"match": {"row": "*", "col": "0"}, "proc": "origin"},
			{"match": {"row": "*", "col": "cols-1"}, "proc": "reflect"},
			{"match": {"default": true}, "proc": "relay"}
		]
	}`)
	rows, cols, err := inferGridSize(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if cols < 2 {
		t.Fatalf("got %dx%d, cols must be >=2 so col 0 != col cols-1 (origin/reflect must not collide)", rows, cols)
	}
	// 6x7 is already collision-free for this manifest too.
	if rows != 6 || cols != 7 {
		t.Errorf("got %dx%d, want the default 6x7", rows, cols)
	}
}

// TestInferGridSizeWildcardCollision confirms the collision itself is
// still detected when forced smaller than the default -- i.e. fits(...)
// correctly rejects cols=1 for the same-pattern origin/reflect leaves,
// not just that inferGridSize happens to prefer a bigger size.
func TestInferGridSizeWildcardCollision(t *testing.T) {
	byRC := []rcLeaf{
		{colExpr: "0", rowWild: true},
		{colExpr: "cols-1", rowWild: true},
	}
	if fits(byRC, nil, 1, 1) {
		t.Error("fits(1x1) = true, want false: col 0 and col cols-1 both evaluate to 0 at cols=1")
	}
	if !fits(byRC, nil, 1, 2) {
		t.Error("fits(1x2) = false, want true: col 0 and col 1 are distinct")
	}
}

// TestInferGridSizeDifferentWildcardPatternsDontCollide confirms a
// wildcard-one-axis leaf is never compared against a fully-concrete leaf
// -- that's intentional "catch the rest" layering (relay covering
// whatever controller/rowEnd don't), not a bug to avoid.
func TestInferGridSizeDifferentWildcardPatternsDontCollide(t *testing.T) {
	byRC := []rcLeaf{
		{rowExpr: "0", colExpr: "0"},  // controller: fully concrete
		{rowExpr: "0", colWild: true}, // relay: wildcards col only
	}
	if !fits(byRC, nil, 1, 1) {
		t.Error("fits(1x1) = false, want true: a fully-concrete leaf and a partial-wildcard leaf overlapping is intentional, not a collision")
	}
}

func TestInferGridSizeFlatID(t *testing.T) {
	manifest := []byte(`{
		"leaves": [
			{"match": {"id": "0"}, "proc": "boot"},
			{"match": {"id": "5"}, "proc": "special"},
			{"match": {"default": true}, "proc": "idle"}
		]
	}`)
	rows, cols, err := inferGridSize(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if rows*cols <= 5 {
		t.Errorf("got %dx%d (=%d nodes), want enough nodes to fit id 5", rows, cols, rows*cols)
	}
}

func TestEvalGridExpr(t *testing.T) {
	cases := []struct {
		expr       string
		rows, cols int
		want       int
	}{
		{"0", 3, 4, 0},
		{"cols-1", 3, 4, 3},
		{"rows-1", 3, 4, 2},
		{"cols - 2", 3, 4, 2},
	}
	for _, c := range cases {
		got, err := evalGridExpr(c.expr, c.rows, c.cols)
		if err != nil {
			t.Errorf("evalGridExpr(%q): %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("evalGridExpr(%q, rows=%d, cols=%d) = %d, want %d", c.expr, c.rows, c.cols, got, c.want)
		}
	}
}
