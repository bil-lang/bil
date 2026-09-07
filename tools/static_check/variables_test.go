package main

import (
	"go/token"
	"testing"
)

func TestCheckSharedVariables(t *testing.T) {
	cases := []struct {
		file      string
		wantCount int
	}{
		{"testdata/var_clean.go", 0},
		{"testdata/var_violation.go", 1},
		{"testdata/var_safe_by_value.go", 0},
		// Package-level variable touched only inside called functions, no
		// parameter involved — invisible before the interprocedural
		// recursion fix, must be caught now.
		{"testdata/var_package_global.go", 1},
		// A struct with a slice field must stay out of scope (deferred,
		// not silently trusted) even though it's passed by value.
		{"testdata/var_struct_with_slice_field.go", 0},
		// &x taken in one branch, x used in another — the generalized
		// version of bilc's own checkNoPointerParams; previously
		// exercised only ad hoc, never as a committed regression fixture.
		{"testdata/var_address_taken.go", 1},
		// &x taken in *both* branches — bilc's own checkNoPointerParams
		// (since removed, protection now lives here only) flagged this
		// symmetric case too; 2 conflicts, one reported per direction.
		{"testdata/var_address_taken_both.go", 2},
		// Existing channel-only fixtures must stay unaffected by this rule
		// (no free non-channel variables cross branches in them).
		{"testdata/clean.go", 0},
		// A method call on a free variable's receiver must be treated as a
		// potential write, not just a read — otherwise a mutation reached
		// only through a method call is invisible to this check.
		{"testdata/var_method_mutation.go", 1},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			fset := token.NewFileSet()
			file, info, typeErrs, err := analyzeFile(fset, c.file)
			if err != nil {
				t.Fatalf("analyzeFile: %v", err)
			}
			if len(typeErrs) > 0 {
				t.Fatalf("type errors: %v", typeErrs)
			}

			conflicts := CheckSharedVariables(info, file)
			if len(conflicts) != c.wantCount {
				t.Fatalf("got %d conflicts, want %d: %+v", len(conflicts), c.wantCount, conflicts)
			}
		})
	}
}
