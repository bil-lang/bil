package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
)

// deployManifest mirrors bilc.DeployManifest's JSON shape (see
// tools/bilc/bilc.go's jsonManifest/jsonLeaf/jsonMatch/jsonCondition) --
// duplicated here rather than imported, since those types are unexported
// and the JSON shape is small and stable. Only the fields inferGridSize
// actually needs are kept.
type deployManifest struct {
	Leaves []deployLeaf `json:"leaves"`
}

type deployLeaf struct {
	Match deployMatch `json:"match"`
}

type deployMatch struct {
	ID      string `json:"id,omitempty"`
	Row     string `json:"row,omitempty"`
	Col     string `json:"col,omitempty"`
	Default bool   `json:"default,omitempty"`
}

// maxGridSearch bounds inferGridSize's search -- generous for any
// realistic placed-par program (every example so far needs at most a
// handful of rows/cols), while keeping a runaway search bounded.
const maxGridSearch = 32

// concreteRC/concreteID are the two shapes of "explicit, must-be-distinct"
// leaf position inferGridSize's search cares about -- see its own doc
// comment for why default/wildcard leaves are excluded entirely.
type concreteRC struct{ rowExpr, colExpr string }
type concreteID struct{ idExpr string }

// inferGridSize computes the smallest rows x cols grid at which every
// explicit (non-default, non-wildcard) leaf in a DeployManifest resolves
// to its own distinct (row, col) cell -- see the emu command's design
// notes for why this is computed rather than guessed or required as a
// flag. A leaf wildcarding one axis (`processor(0, *)`) or fully wild
// (recorded as Default) never needs a distinct cell of its own: by
// construction it's meant to cover whatever a more specific leaf doesn't,
// exactly like the generated Go switch's own case ordering already
// handles today. Leaves are otherwise addressed by flat id
// (`processor(ID)`, checked against id values directly, not converted to
// row/col) or by row/col pair.
//
// Returns an error only if no size within maxGridSearch works, or a
// match/id expression fails to parse/evaluate -- both should be
// unreachable for any real bilc-generated manifest.
func inferGridSize(manifest []byte) (rows, cols int, err error) {
	var dm deployManifest
	if err := json.Unmarshal(manifest, &dm); err != nil {
		return 0, 0, fmt.Errorf("parsing deploy manifest: %w", err)
	}

	var byRC []concreteRC
	var byID []concreteID
	for _, leaf := range dm.Leaves {
		m := leaf.Match
		switch {
		case m.Default:
			continue
		case m.ID != "":
			byID = append(byID, concreteID{m.ID})
		case m.Row == "*" || m.Col == "*":
			continue // partial wildcard -- covers whatever a more specific leaf doesn't
		default:
			byRC = append(byRC, concreteRC{m.Row, m.Col})
		}
	}
	if len(byRC) == 0 && len(byID) == 0 {
		return 1, 1, nil // nothing explicit to distinguish -- e.g. a placed-par with only a default clause
	}

	for cols := 1; cols <= maxGridSearch; cols++ {
		for rows := 1; rows <= maxGridSearch; rows++ {
			if fits(byRC, byID, rows, cols) {
				return rows, cols, nil
			}
		}
	}
	return 0, 0, fmt.Errorf("no grid up to %dx%d distinguishes every placed processor -- pass -rows/-cols explicitly", maxGridSearch, maxGridSearch)
}

func fits(byRC []concreteRC, byID []concreteID, rows, cols int) bool {
	seen := map[[2]int]bool{}
	for _, l := range byRC {
		r, err := evalGridExpr(l.rowExpr, rows, cols)
		if err != nil {
			return false
		}
		c, err := evalGridExpr(l.colExpr, rows, cols)
		if err != nil {
			return false
		}
		if r < 0 || r >= rows || c < 0 || c >= cols {
			return false
		}
		key := [2]int{r, c}
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	seenID := map[int]bool{}
	for _, l := range byID {
		id, err := evalGridExpr(l.idExpr, rows, cols)
		if err != nil {
			return false
		}
		if id < 0 || id >= rows*cols {
			return false
		}
		if seenID[id] {
			return false
		}
		seenID[id] = true
	}
	return true
}

// evalGridExpr evaluates a bilc match/id expression (arithmetic only --
// int literals, `rows`/`cols` identifiers, `+ - * /`, parens; never the
// comparison/boolean operators a leaf's separate if/else conditions use,
// since match expressions are themselves never boolean) against a
// candidate grid size, using go/parser rather than a hand-rolled
// tokenizer since the grammar is exactly a subset of real Go expression
// syntax.
func evalGridExpr(expr string, rows, cols int) (int, error) {
	node, err := parser.ParseExpr(expr)
	if err != nil {
		return 0, fmt.Errorf("parsing %q: %w", expr, err)
	}
	return evalGridNode(node, rows, cols)
}

func evalGridNode(n ast.Expr, rows, cols int) (int, error) {
	switch n := n.(type) {
	case *ast.BasicLit:
		if n.Kind != token.INT {
			return 0, fmt.Errorf("non-integer literal %q", n.Value)
		}
		var v int
		if _, err := fmt.Sscanf(n.Value, "%d", &v); err != nil {
			return 0, err
		}
		return v, nil
	case *ast.Ident:
		switch n.Name {
		case "rows":
			return rows, nil
		case "cols":
			return cols, nil
		}
		return 0, fmt.Errorf("unknown identifier %q in match expression", n.Name)
	case *ast.ParenExpr:
		return evalGridNode(n.X, rows, cols)
	case *ast.UnaryExpr:
		x, err := evalGridNode(n.X, rows, cols)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.SUB:
			return -x, nil
		case token.ADD:
			return x, nil
		}
		return 0, fmt.Errorf("unsupported unary operator %v in match expression", n.Op)
	case *ast.BinaryExpr:
		x, err := evalGridNode(n.X, rows, cols)
		if err != nil {
			return 0, err
		}
		y, err := evalGridNode(n.Y, rows, cols)
		if err != nil {
			return 0, err
		}
		switch n.Op {
		case token.ADD:
			return x + y, nil
		case token.SUB:
			return x - y, nil
		case token.MUL:
			return x * y, nil
		case token.QUO:
			if y == 0 {
				return 0, fmt.Errorf("division by zero in match expression")
			}
			return x / y, nil
		}
		return 0, fmt.Errorf("unsupported operator %v in match expression", n.Op)
	default:
		return 0, fmt.Errorf("unsupported expression shape %T in match expression", n)
	}
}
