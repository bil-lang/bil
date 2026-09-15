// Package vet type-checks a Go file (bilc's transformed output, or any Go
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
package vet

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// analyzeFile parses and type-checks the Go source at path, resolving its
// imports via golang.org/x/tools/go/packages (a real, module-aware
// resolver -- it shells out to the `go` command itself, the same one
// `go build` would use) rather than go/importer's old "source" mode,
// which only ever understood classic GOPATH-style directory lookup and
// could never resolve a real module import like `emulator/bilink` (the
// sibling emulator repo, its own separate go.mod-based module).
//
// path is staged into a fresh, private temp directory of its own (see
// analyzeFileWithModule) rather than analyzed in place: go/packages
// resolves a whole package (every .go file in a directory), not one
// file in isolation the way the old parser.ParseFile-based analyzeFile
// did, and path's own directory can't be assumed to contain nothing
// else relevant -- confirmed directly: this file's own test suite keeps
// dozens of unrelated single-file fixtures side by side in testdata/,
// each meant to be checked as if it were the only file in the world.
// Staging a private copy elsewhere restores that isolation.
//
// typeErrs carries any type-checking errors (collected, not fatal
// individually -- go/types keeps checking and populating info as far as
// it can), exactly matching the previous go/importer-based behavior's
// contract with Check below.
func analyzeFile(fset *token.FileSet, path string) (file *ast.File, info *types.Info, typeErrs []error, err error) {
	return analyzeFileWithModule(fset, path, nil)
}

// analyzeFileWithModule is analyzeFile, plus extraGoMod: zero or more
// additional lines appended to the generated go.mod's body -- e.g. a
// `require`/`replace` pair giving path's own import of a real external
// module (the sibling emulator repo, say) something to resolve against.
// Check uses this to support a placed/link program's `import
// "emulator/bilink"`; every other caller (including every existing test
// in this package, via plain analyzeFile) needs nothing beyond the
// standard library, so they get a bare go.mod with no extraGoMod at all.
func analyzeFileWithModule(fset *token.FileSet, path string, extraGoMod []string) (file *ast.File, info *types.Info, typeErrs []error, err error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	dir, err := os.MkdirTemp("", "bilvet-*")
	if err != nil {
		return nil, nil, nil, err
	}
	defer os.RemoveAll(dir)

	stagedPath := filepath.Join(dir, filepath.Base(path))
	if err := os.WriteFile(stagedPath, src, 0o644); err != nil {
		return nil, nil, nil, err
	}
	goMod := "module bilcheck\n\ngo 1.25.0\n"
	if len(extraGoMod) > 0 {
		goMod += "\n" + strings.Join(extraGoMod, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		return nil, nil, nil, err
	}

	return loadOneFilePackage(fset, dir)
}

// analyzeFileInPlace is analyzeFile, but without any of its staging: it
// points go/packages directly at path's own directory, trusting the
// caller that doing so is actually safe -- that directory must already
// resolve everything path imports via a real go.mod somewhere above it
// (not one this package generates), and must not contain any other,
// unrelated .go file that would become part of the same package by
// accident. tools/bil's `bil emu` uses this for a placed program's
// per-role scratch directory (emulatorDir/nodeprog/bilemu-*/roles/<role>/):
// each holds exactly one file, and already sits inside the real emulator
// module, so emulator/bilink resolves there via the module's own
// ordinary self-import -- copying it elsewhere first, the way
// analyzeFileWithModule does, would only throw that away.
func analyzeFileInPlace(fset *token.FileSet, path string) (file *ast.File, info *types.Info, typeErrs []error, err error) {
	return loadOneFilePackage(fset, filepath.Dir(path))
}

// loadOneFilePackage is analyzeFile{WithModule,InPlace}'s shared tail:
// load dir as a single Go package via go/packages and pull out its lone
// file's syntax tree, type info, and any errors -- dir must already
// resolve to exactly one Go file, one way or another (analyzeFileWithModule
// arranges that by construction; analyzeFileInPlace trusts its caller to
// have arranged it).
func loadOneFilePackage(fset *token.FileSet, dir string) (file *ast.File, info *types.Info, typeErrs []error, err error) {
	cfg := &packages.Config{
		Mode: packages.LoadAllSyntax,
		Dir:  dir,
		Fset: fset,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		return nil, nil, nil, err
	}
	if len(pkgs) != 1 {
		return nil, nil, nil, fmt.Errorf("expected exactly one package in %s, got %d", dir, len(pkgs))
	}
	pkg := pkgs[0]
	if len(pkg.Syntax) != 1 {
		return nil, nil, nil, fmt.Errorf("expected exactly one file in package %s, got %d", pkg.PkgPath, len(pkg.Syntax))
	}
	for _, e := range pkg.Errors {
		typeErrs = append(typeErrs, e)
	}

	return pkg.Syntax[0], pkg.TypesInfo, typeErrs, nil
}

// Check parses, type-checks, and runs all of Bil's usage checks against the
// Go file at path. extraGoMod, if given, is passed straight through to
// analyzeFileWithModule -- extra `require`/`replace` lines for the
// generated go.mod path is staged alongside, needed when path imports a
// real external module (see tools/bil/main.go's runCmd, which passes a
// `replace emulator => ...` line for a placed/link program). Omit it
// entirely for an ordinary program that only needs the standard library.
//
// err is non-nil only when the file couldn't be analyzed at all (I/O or
// parse failure) — a file that parses and type-checks but fails one or
// more Bil rules returns a nil err with a non-empty messages slice.
// Each message is a fully formatted, ready-to-print line (position,
// description, and rationale). ok := err == nil && len(messages) == 0.
func Check(path string, extraGoMod ...string) (messages []string, err error) {
	fset := token.NewFileSet()
	file, info, typeErrs, err := analyzeFileWithModule(fset, path, extraGoMod)
	if err != nil {
		return nil, err
	}
	return runChecks(fset, file, info, typeErrs), nil
}

// CheckInPlace is Check, but for a file whose own directory already
// resolves everything it imports via a real go.mod somewhere above it
// (see analyzeFileInPlace's own doc comment for the exact contract) --
// `bil emu` uses this for a placed program's per-role scratch
// directories, which already sit inside the real emulator module and so
// need no generated go.mod/replace the way Check's bare-temp-file
// callers do.
func CheckInPlace(path string) (messages []string, err error) {
	fset := token.NewFileSet()
	file, info, typeErrs, err := analyzeFileInPlace(fset, path)
	if err != nil {
		return nil, err
	}
	return runChecks(fset, file, info, typeErrs), nil
}

// runChecks is Check/CheckInPlace's shared tail once a file's syntax
// tree and type info are in hand: report any type errors as messages
// (a program that doesn't even type-check has nothing further worth
// checking), otherwise run all six Bil usage checks and format their
// findings into the same ready-to-print message shape.
func runChecks(fset *token.FileSet, file *ast.File, info *types.Info, typeErrs []error) (messages []string) {
	if len(typeErrs) > 0 {
		for _, e := range typeErrs {
			messages = append(messages, e.Error())
		}
		return messages
	}

	conflicts, closeSendConflicts := CheckChannelUsage(info, file)
	leaks := CheckChannelConfinement(info, file)
	leaks = append(leaks, CheckNoChannelOfChannel(file)...)
	varConflicts := CheckSharedVariables(info, file)
	refConflicts := CheckRefSharing(info, file)
	arrayConflicts, appendViolations := CheckArraySharing(info, file)

	for _, c := range conflicts {
		if c.Dir == dirClose {
			messages = append(messages, fmt.Sprintf(
				"%s: channel %s is closed in more than one par branch (branch %d, also branch %d at %s) — closing a channel twice panics; a channel should be closed by exactly one component of a parallel",
				fset.Position(c.FirstPos), c.Identity.Name(), c.FirstFrom+1, c.SecondFrom+1, fset.Position(c.SecondPos),
			))
			continue
		}
		verb := "sent to"
		noun := "output"
		if c.Dir == dirRecv {
			verb = "received from"
			noun = "input"
		}
		messages = append(messages, fmt.Sprintf(
			"%s: channel %s is %s in more than one par branch (branch %d, also branch %d at %s) — a channel may only be used for %s in one component of a parallel",
			fset.Position(c.FirstPos), c.Identity.Name(), verb, c.FirstFrom+1, c.SecondFrom+1, fset.Position(c.SecondPos), noun,
		))
	}
	for _, cs := range closeSendConflicts {
		messages = append(messages, fmt.Sprintf(
			"%s: channel %s is closed here (branch %d) while also sent to in a different par branch (branch %d, at %s) — a send on a closed channel panics",
			fset.Position(cs.ClosePos), cs.Identity.Name(), cs.CloseFrom+1, cs.SendFrom+1, fset.Position(cs.SendPos),
		))
	}
	for _, l := range leaks {
		messages = append(messages, fmt.Sprintf("%s: %s", fset.Position(l.Pos), l.Message()))
	}
	for _, v := range varConflicts {
		messages = append(messages, fmt.Sprintf(
			"%s: variable %s written here (branch %d) is also used in a concurrent par branch (branch %d, also at %s) — a variable changed by input or assignment in one component of a parallel may not be used, read or written, in any other component",
			fset.Position(v.WritePos), v.Obj.Name(), v.WriteBranch+1, v.UseBranch+1, fset.Position(v.UsePos),
		))
	}
	for _, r := range refConflicts {
		messages = append(messages, fmt.Sprintf("%s: %s", fset.Position(r.FirstPos), r.Message(fset)))
	}
	for _, a := range arrayConflicts {
		if a.WriteBranch == a.UseBranch {
			messages = append(messages, fmt.Sprintf(
				"%s: array/slice region written here (branch %d, replicated) may overlap a different concurrent instantiation of the same replicated construct — components of an array may be assigned to in parallel only if it can be determined at compile time that the used subscripts select disjoint components",
				fset.Position(a.WritePos), a.WriteBranch+1,
			))
			continue
		}
		messages = append(messages, fmt.Sprintf(
			"%s: array/slice region written here (branch %d) may overlap a region touched in a concurrent par branch (branch %d, also at %s) — components of an array may be assigned to in parallel only if it can be determined at compile time that the used subscripts select distinct components",
			fset.Position(a.WritePos), a.WriteBranch+1, a.UseBranch+1, fset.Position(a.UsePos),
		))
	}
	for _, av := range appendViolations {
		messages = append(messages, fmt.Sprintf(
			"%s: append on a shared array/slice region whose capacity isn't known bounded to its own length — this can silently overwrite a neighboring region without reallocating; slice with a full/three-index expression (s[lo:hi:hi]) to bound its capacity",
			fset.Position(av.Pos),
		))
	}
	return messages
}
