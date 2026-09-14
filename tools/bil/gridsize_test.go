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
// wildcarding the rest of row 0, idle wildcarding everything else. Only the
// two concrete leaves should drive sizing.
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
	if rows != 1 || cols != 2 {
		t.Errorf("got %dx%d, want 1x2 (smallest grid distinguishing controller at col 0 from rowEnd at col cols-1)", rows, cols)
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
