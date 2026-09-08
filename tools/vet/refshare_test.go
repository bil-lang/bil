package vet

import (
	"go/token"
	"testing"
)

func TestCheckRefSharing(t *testing.T) {
	cases := []struct {
		file      string
		wantCount int
	}{
		{"testdata/ref_clean.go", 0},
		{"testdata/ref_violation_pointer.go", 1},
		{"testdata/ref_violation_map.go", 1},
		{"testdata/ref_package_global_pointer.go", 1},
		// Existing fixtures from other rules must stay unaffected — none
		// of them share a pointer/map/interface.
		{"testdata/clean.go", 0},
		{"testdata/var_clean.go", 0},
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

			conflicts := CheckRefSharing(info, file)
			if len(conflicts) != c.wantCount {
				t.Fatalf("got %d conflicts, want %d: %+v", len(conflicts), c.wantCount, conflicts)
			}
		})
	}
}
