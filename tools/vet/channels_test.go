package vet

import (
	"go/token"
	"testing"
)

func TestCheckChannelUsage(t *testing.T) {
	cases := []struct {
		file          string
		wantCount     int
		wantCloseSend int
	}{
		{"testdata/clean.go", 0, 0},
		{"testdata/violation_procs.go", 1, 0},
		{"testdata/violation_bare.go", 1, 0},
		// Send-side mirror of violation_procs.go — closes the regression
		// gap left by dropping bilc's own chan-shared-output.bil fixture.
		{"testdata/violation_procs_send.go", 1, 0},
		// Undirected chan param, one sender, one receiver — legal, must
		// NOT be flagged. Closes the regression gap left by dropping
		// bilc's own chan-shared-undirected.bil fixture (which was a
		// false positive, now fixed).
		{"testdata/chan_undirected_clean.go", 0, 0},
		// Package-level channel touched only inside called functions, no
		// parameter involved — invisible before the interprocedural isFree
		// generalization, must be caught now.
		{"testdata/chan_package_global.go", 1, 0},
		{"testdata/chan_array_static_distinct.go", 0, 0},
		{"testdata/chan_array_static_same_lit.go", 1, 0},
		{"testdata/chan_array_replicated_send_sequential_recv.go", 0, 0},
		{"testdata/chan_array_opaque_vs_lit.go", 1, 0},
		// Bil's timer exemption — same shape
		// as violation_bare.go, but must NOT be flagged since the channel
		// is a timer, not an ordinary one.
		{"testdata/chan_timer_multi_recv.go", 0, 0},
		// close/close is a same-direction conflict like send/send or
		// recv/recv — double close panics.
		{"testdata/chan_close_twice.go", 1, 0},
		// close/send is asymmetric — reported via CloseSendConflict, not
		// ChannelConflict.
		{"testdata/chan_close_vs_send.go", 0, 1},
		// close/receive is the standard completion idiom — must NOT be
		// flagged either way.
		{"testdata/chan_close_vs_recv.go", 0, 0},
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
			conflicts, closeSend := CheckChannelUsage(info, file)
			if len(conflicts) != c.wantCount {
				t.Fatalf("got %d conflicts, want %d: %+v", len(conflicts), c.wantCount, conflicts)
			}
			if len(closeSend) != c.wantCloseSend {
				t.Fatalf("got %d close/send conflicts, want %d: %+v", len(closeSend), c.wantCloseSend, closeSend)
			}
		})
	}
}
