package vet

import (
	"go/token"
	"testing"
)

func TestCheckArraySharing(t *testing.T) {
	cases := []struct {
		file          string
		wantConflicts int
		wantAppends   int
	}{
		{"testdata/region_splitn_static.go", 0, 0},
		{"testdata/region_splitn_replicated.go", 0, 0},
		{"testdata/region_splitn_remainder.go", 0, 0},
		{"testdata/region_custom_split.go", 0, 0},
		{"testdata/region_handwritten_strided.go", 0, 0},
		{"testdata/region_mid_mip_same.go", 0, 0},
		// Both branches genuinely write overlapping regions here, so both
		// directions legitimately conflict — same symmetric-reporting
		// convention channels.go/variables.go already use for a mutual
		// write-write pair, not double-counting.
		{"testdata/region_mid_mip_drift.go", 2, 0},
		{"testdata/region_overlap.go", 2, 0},
		{"testdata/region_readonly.go", 0, 0},
		{"testdata/region_append_uncapped.go", 0, 1},
		{"testdata/region_replicated_overlap.go", 1, 0},
		{"testdata/region_selfappend_uncapped.go", 0, 1},
		{"testdata/region_selfappend_capped.go", 0, 0},
		{"testdata/region_chunk_subslice_write.go", 0, 0},
		// Both branches write (via copy) into overlapping windows — same
		// mutual write-write symmetric-reporting shape as region_overlap.go.
		{"testdata/region_copy_violation.go", 2, 0},
		{"testdata/region_copy_safe.go", 0, 0},
		{"testdata/region_reslice_violation.go", 2, 0},
		{"testdata/region_grid_static.go", 0, 0},
		{"testdata/region_grid_replicated.go", 0, 0},
		{"testdata/region_grid_row_disjoint_col_same.go", 0, 0},
		// The broken helper's own self-consistency check fires once, on
		// the one written region representing every (i,j) instantiation
		// at once — same shape as region_replicated_overlap.go's single
		// self-conflict entry.
		{"testdata/region_grid_broken_stride.go", 1, 0},
		// Both branches genuinely write the same (i=0,j=0) tile, so both
		// directions legitimately conflict — same symmetric-reporting
		// convention region_overlap.go/region_copy_violation.go use.
		{"testdata/region_grid_repeated_tile.go", 2, 0},
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

			conflicts, appends := CheckArraySharing(info, file)
			if len(conflicts) != c.wantConflicts {
				t.Fatalf("got %d conflicts, want %d: %+v", len(conflicts), c.wantConflicts, conflicts)
			}
			if len(appends) != c.wantAppends {
				t.Fatalf("got %d append violations, want %d: %+v", len(appends), c.wantAppends, appends)
			}
		})
	}
}
