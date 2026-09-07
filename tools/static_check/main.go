// static_check type-checks a Go file (bilc's transformed output, or any Go
// source using the same par(branches ...func()) shape) and reports
// channel- and variable-usage violations: point-to-point direction
// conflicts across par branches (channels.go), confinement leaks — a
// channel value escaping through assignment, struct-literal storage, a
// return, or as another channel's payload (confinement.go) — the general
// variable-usage rule — a plain variable written in one par branch and
// used, read or written, in another (variables.go) — a ban on sharing a
// pointer, map, or interface value across par branches at all
// (refshare.go) — and array/slice disjointness, proven rather than
// trusted (regions.go).
package main

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
)

// analyzeFile parses and type-checks the Go source at path. typeErrs
// carries any type-checking errors (collected, not fatal individually —
// go/types keeps checking and populating info as far as it can).
func analyzeFile(fset *token.FileSet, path string) (file *ast.File, info *types.Info, typeErrs []error, err error) {
	file, err = parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		return nil, nil, nil, err
	}

	info = &types.Info{
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
		Types: make(map[ast.Expr]types.TypeAndValue),
	}
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "source", nil),
		Error: func(err error) {
			typeErrs = append(typeErrs, err)
		},
	}
	conf.Check(file.Name.Name, fset, []*ast.File{file}, info)

	return file, info, typeErrs, nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: static_check <file.go>")
		os.Exit(2)
	}
	path := os.Args[1]

	fset := token.NewFileSet()
	file, info, typeErrs, err := analyzeFile(fset, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(typeErrs) > 0 {
		for _, e := range typeErrs {
			fmt.Fprintln(os.Stderr, e)
		}
		os.Exit(1)
	}

	conflicts, closeSendConflicts := CheckChannelUsage(info, file)
	leaks := CheckChannelConfinement(info, file)
	leaks = append(leaks, CheckNoChannelOfChannel(file)...)
	varConflicts := CheckSharedVariables(info, file)
	refConflicts := CheckRefSharing(info, file)
	arrayConflicts, appendViolations := CheckArraySharing(info, file)

	if len(conflicts) == 0 && len(closeSendConflicts) == 0 && len(leaks) == 0 && len(varConflicts) == 0 && len(refConflicts) == 0 &&
		len(arrayConflicts) == 0 && len(appendViolations) == 0 {
		return
	}

	for _, c := range conflicts {
		if c.Dir == dirClose {
			fmt.Printf(
				"%s: channel %s is closed in more than one par branch (branch %d, also branch %d at %s) — closing a channel twice panics; a channel should be closed by exactly one component of a parallel\n",
				fset.Position(c.FirstPos), c.Identity.Name(), c.FirstFrom+1, c.SecondFrom+1, fset.Position(c.SecondPos),
			)
			continue
		}
		verb := "sent to"
		noun := "output"
		if c.Dir == dirRecv {
			verb = "received from"
			noun = "input"
		}
		fmt.Printf(
			"%s: channel %s is %s in more than one par branch (branch %d, also branch %d at %s) — a channel may only be used for %s in one component of a parallel\n",
			fset.Position(c.FirstPos), c.Identity.Name(), verb, c.FirstFrom+1, c.SecondFrom+1, fset.Position(c.SecondPos), noun,
		)
	}
	for _, cs := range closeSendConflicts {
		fmt.Printf(
			"%s: channel %s is closed here (branch %d) while also sent to in a different par branch (branch %d, at %s) — a send on a closed channel panics\n",
			fset.Position(cs.ClosePos), cs.Identity.Name(), cs.CloseFrom+1, cs.SendFrom+1, fset.Position(cs.SendPos),
		)
	}
	for _, l := range leaks {
		fmt.Printf("%s: %s\n", fset.Position(l.Pos), l.Message())
	}
	for _, v := range varConflicts {
		fmt.Printf(
			"%s: variable %s written here (branch %d) is also used in a concurrent par branch (branch %d, also at %s) — a variable changed by input or assignment in one component of a parallel may not be used, read or written, in any other component\n",
			fset.Position(v.WritePos), v.Obj.Name(), v.WriteBranch+1, v.UseBranch+1, fset.Position(v.UsePos),
		)
	}
	for _, r := range refConflicts {
		fmt.Printf("%s: %s\n", fset.Position(r.FirstPos), r.Message(fset))
	}
	for _, a := range arrayConflicts {
		if a.WriteBranch == a.UseBranch {
			fmt.Printf(
				"%s: array/slice region written here (branch %d, replicated) may overlap a different concurrent instantiation of the same replicated construct — components of an array may be assigned to in parallel only if it can be determined at compile time that the used subscripts select disjoint components\n",
				fset.Position(a.WritePos), a.WriteBranch+1,
			)
			continue
		}
		fmt.Printf(
			"%s: array/slice region written here (branch %d) may overlap a region touched in a concurrent par branch (branch %d, also at %s) — components of an array may be assigned to in parallel only if it can be determined at compile time that the used subscripts select distinct components\n",
			fset.Position(a.WritePos), a.WriteBranch+1, a.UseBranch+1, fset.Position(a.UsePos),
		)
	}
	for _, av := range appendViolations {
		fmt.Printf(
			"%s: append on a shared array/slice region whose capacity isn't known bounded to its own length — this can silently overwrite a neighboring region without reallocating; slice with a full/three-index expression (s[lo:hi:hi]) to bound its capacity\n",
			fset.Position(av.Pos),
		)
	}
	os.Exit(1)
}
