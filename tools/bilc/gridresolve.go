// Copyright (c) 2026 Stephen Roe. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bilc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// This file backs `bil run --dry-run`: resolving a `placed par` block's
// leaves against a concrete -rows/-cols grid, entirely at bilc transform
// time, with no real or emulated hardware involved. Both pieces here are
// direct ports of logic that already exists twice in the sibling emulator
// repo (../../emulator) for the real grid launcher -- a third port, not a
// new design:
//
//   - dryRunEvalExpr/dryRunResolveLeaf mirror
//     emulator/cmd/multicore/resolve.go's evalExpr/resolveRole (itself a Go
//     port of emulator/cmd/wasm/static/index.html's own evalExpr/
//     resolveRole) -- the same restricted expression grammar a placed-par
//     clause match or if/else condition is required to use: identifiers
//     r/c/rows/cols, int literals, +, -, ==, !=, &&.
//   - dryRunMeshEdges mirrors emulator/cmd/multicore/topology.go's
//     buildTopology -- same fixed north/east/south/west slot convention,
//     same plain-mesh (no torus) adjacency, just allocating a local Go
//     channel per directed edge instead of a Unix-domain-socket path.

// dryRunGrid selects --dry-run's grid size -- see transformer.dryRunGrid's
// own doc comment and TransformDryRun.
type dryRunGrid struct {
	rows, cols int
}

// Fixed link-slot convention, shared with the real launcher (see
// emulator/cmd/multicore/topology.go and cmd/wasm/static/index.html's own
// buildTopology): a node's link[i] slots are 0=north, 1=east, 2=south,
// 3=west.
const (
	dryRunNorth        = 0
	dryRunEast         = 1
	dryRunSouth        = 2
	dryRunWest         = 3
	dryRunNumLinkSlots = 4
)

// dryRunEdge is one directed data-flow edge of the rows*cols mesh: node
// (r1,c1)'s link[slot1] (its Send/OUT side) feeds node (r2,c2)'s
// link[slot2] (its Recv/IN side).
type dryRunEdge struct {
	r1, c1, slot1 int
	r2, c2, slot2 int
}

// dryRunMeshEdges enumerates every directed edge of a plain rows*cols
// mesh -- a direct port of buildTopology's own wire() call sequence, minus
// the socket-path allocation (dry-run wires a local Go channel per edge
// instead; see emitPlacedParClausesDryRun).
func dryRunMeshEdges(rows, cols int) []dryRunEdge {
	var edges []dryRunEdge
	for r := 0; r < rows; r++ {
		for c := 0; c < cols-1; c++ {
			edges = append(edges, dryRunEdge{r, c, dryRunEast, r, c + 1, dryRunWest})
			edges = append(edges, dryRunEdge{r, c + 1, dryRunWest, r, c, dryRunEast})
		}
	}
	for r := 0; r < rows-1; r++ {
		for c := 0; c < cols; c++ {
			edges = append(edges, dryRunEdge{r, c, dryRunSouth, r + 1, c, dryRunNorth})
			edges = append(edges, dryRunEdge{r + 1, c, dryRunNorth, r, c, dryRunSouth})
		}
	}
	return edges
}

// dryRunResolveLeaf is a direct port of resolve.go's resolveRole: walk
// leaves in order (first match wins, matching bilc's own switch/if-else
// semantics) and return the leaf that should run at (r,c) in a rows*cols
// grid.
func dryRunResolveLeaf(leaves []resolvedLeaf, r, c, rows, cols int) (*resolvedLeaf, error) {
	vars := map[string]int{"r": r, "c": c, "rows": rows, "cols": cols}
	for i := range leaves {
		l := &leaves[i]
		matched, err := dryRunClauseMatches(l.clause, vars, r, c)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		ok := true
		for _, cond := range l.conditions {
			v, err := dryRunEvalBool(cond.exprSrc, vars)
			if err != nil {
				return nil, err
			}
			if cond.negate {
				v = !v
			}
			if !v {
				ok = false
				break
			}
		}
		if ok {
			return l, nil
		}
	}
	return nil, fmt.Errorf("--dry-run: no placed-par leaf matches (r=%d, c=%d) at rows=%d cols=%d -- try larger -rows/-cols, or add a `processor(*, *)` catch-all", r, c, rows, cols)
}

func dryRunClauseMatches(cl placedClause, vars map[string]int, r, c int) (bool, error) {
	switch {
	case cl.isFullyWild():
		return true, nil
	case cl.idExpr != "":
		v, err := dryRunEvalInt(cl.idExpr, vars)
		if err != nil {
			return false, err
		}
		return v == r*vars["cols"]+c, nil
	default:
		rowOK := true
		if cl.rowExpr != "*" {
			v, err := dryRunEvalInt(cl.rowExpr, vars)
			if err != nil {
				return false, err
			}
			rowOK = v == r
		}
		colOK := true
		if cl.colExpr != "*" {
			v, err := dryRunEvalInt(cl.colExpr, vars)
			if err != nil {
				return false, err
			}
			colOK = v == c
		}
		return rowOK && colOK, nil
	}
}

func dryRunEvalBool(src string, vars map[string]int) (bool, error) {
	v, err := dryRunEvalExpr(src, vars)
	if err != nil {
		return false, err
	}
	switch v := v.(type) {
	case bool:
		return v, nil
	case int:
		return v != 0, nil
	default:
		return false, fmt.Errorf("--dry-run: %q did not evaluate to a boolean", src)
	}
}

func dryRunEvalInt(src string, vars map[string]int) (int, error) {
	v, err := dryRunEvalExpr(src, vars)
	if err != nil {
		return 0, err
	}
	switch v := v.(type) {
	case int:
		return v, nil
	case bool:
		return 0, fmt.Errorf("--dry-run: %q evaluated to a boolean, want an int", src)
	default:
		return 0, fmt.Errorf("--dry-run: %q did not evaluate to an int", src)
	}
}

// dryRunEvalExpr evaluates the tiny grammar placed-par clause-match/leaf-
// condition expressions are restricted to -- identifiers (r, c, rows,
// cols), int literals, ==, !=, &&, +, -. Returns int or bool. Ported from
// resolve.go's own evalExpr (itself ported from cmd/wasm/static/index.html) --
// see this file's own top comment.
func dryRunEvalExpr(src string, vars map[string]int) (any, error) {
	p := &dryRunExprParser{src: src, vars: vars}
	v, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	p.skipWs()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("--dry-run: trailing input in %q", p.src)
	}
	return v, nil
}

type dryRunExprParser struct {
	src  string
	pos  int
	vars map[string]int
}

var dryRunIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
var dryRunNumRe = regexp.MustCompile(`^[0-9]+`)

func (p *dryRunExprParser) skipWs() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *dryRunExprParser) consume(tok string) bool {
	p.skipWs()
	if strings.HasPrefix(p.src[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

func dryRunAsInt(v any, src string) (int, error) {
	n, ok := v.(int)
	if !ok {
		return 0, fmt.Errorf("--dry-run: expected an int in %q", src)
	}
	return n, nil
}

func (p *dryRunExprParser) parsePrimary() (any, error) {
	p.skipWs()
	if p.consume("(") {
		v, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		if !p.consume(")") {
			return nil, fmt.Errorf("--dry-run: expected ) in %q", p.src)
		}
		return v, nil
	}
	if m := dryRunIdentRe.FindString(p.src[p.pos:]); m != "" {
		p.pos += len(m)
		v, ok := p.vars[m]
		if !ok {
			return nil, fmt.Errorf("--dry-run: unknown identifier %q in %q", m, p.src)
		}
		return v, nil
	}
	if m := dryRunNumRe.FindString(p.src[p.pos:]); m != "" {
		p.pos += len(m)
		n, _ := strconv.Atoi(m)
		return n, nil
	}
	return nil, fmt.Errorf("--dry-run: unexpected token at %d in %q", p.pos, p.src)
}

func (p *dryRunExprParser) parseAdd() (any, error) {
	lhs, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	v, err := dryRunAsInt(lhs, p.src)
	if err != nil {
		return nil, err
	}
	for {
		if p.consume("+") {
			rhs, err := p.parsePrimary()
			if err != nil {
				return nil, err
			}
			r, err := dryRunAsInt(rhs, p.src)
			if err != nil {
				return nil, err
			}
			v += r
		} else if p.consume("-") {
			rhs, err := p.parsePrimary()
			if err != nil {
				return nil, err
			}
			r, err := dryRunAsInt(rhs, p.src)
			if err != nil {
				return nil, err
			}
			v -= r
		} else {
			return v, nil
		}
	}
}

func (p *dryRunExprParser) parseCmp() (any, error) {
	lhs, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	if p.consume("==") {
		rhs, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		return lhs == rhs, nil
	}
	if p.consume("!=") {
		rhs, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		return lhs != rhs, nil
	}
	return lhs, nil
}

func (p *dryRunExprParser) parseAnd() (any, error) {
	v, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.consume("&&") {
		rhs, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		vb, err := dryRunEvalBoolVal(v, p.src)
		if err != nil {
			return nil, err
		}
		rb, err := dryRunEvalBoolVal(rhs, p.src)
		if err != nil {
			return nil, err
		}
		v = vb && rb
	}
	return v, nil
}

func dryRunEvalBoolVal(v any, src string) (bool, error) {
	switch v := v.(type) {
	case bool:
		return v, nil
	case int:
		return v != 0, nil
	default:
		return false, fmt.Errorf("--dry-run: unsupported value in %q", src)
	}
}
