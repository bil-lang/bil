package main

import (
	"go/token"
	"testing"
)

func TestCheckChannelConfinement(t *testing.T) {
	cases := []struct {
		file     string
		wantKind []LeakKind // kinds that must be present (order-independent); nil means "must be empty"
	}{
		{"testdata/clean.go", nil},
		{"testdata/clean_select_guard.go", nil},
		{"testdata/leak_alias.go", []LeakKind{LeakAlias}},
		{"testdata/leak_alias_reassign.go", []LeakKind{LeakAlias}},
		// leak_composite.go also has a []chan int{c, make(chan int)} slice
		// literal right below the struct one: c (a pre-existing channel)
		// must also be flagged (two LeakComposite entries total), while
		// the fresh make(chan int) element must not be — asserting the
		// exact count, not just "contains LeakComposite", proves both
		// halves of that distinction actually hold, not just one of them.
		{"testdata/leak_composite.go", []LeakKind{LeakComposite, LeakComposite}},
		{"testdata/leak_return.go", []LeakKind{LeakReturn}},
		{"testdata/leak_alias_chan_array.go", []LeakKind{LeakAlias}},
		{"testdata/leak_return_chan_array.go", []LeakKind{LeakReturn}},
		{"testdata/clean_chan_array_direct.go", nil},
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

			leaks := CheckChannelConfinement(info, file)
			assertExactKinds(t, leaks, c.wantKind)
		})
	}
}

func TestCheckNoChannelOfChannel(t *testing.T) {
	cases := []struct {
		file     string
		wantKind []LeakKind
	}{
		{"testdata/clean.go", nil},
		{"testdata/leak_chanofchan.go", []LeakKind{LeakChanOfChan}},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			fset := token.NewFileSet()
			file, _, typeErrs, err := analyzeFile(fset, c.file)
			if err != nil {
				t.Fatalf("analyzeFile: %v", err)
			}
			if len(typeErrs) > 0 {
				t.Fatalf("type errors: %v", typeErrs)
			}

			leaks := CheckNoChannelOfChannel(file)
			assertExactKinds(t, leaks, c.wantKind)
		})
	}
}

// TestLeakChanOfChanFixtureAlsoFlagsPayload confirms leak_chanofchan.go's
// send (of an existing channel over the chan-of-chan) is independently
// caught by CheckChannelConfinement's LeakPayload rule too — belt-and-
// braces alongside the type-level ban.
func TestLeakChanOfChanFixtureAlsoFlagsPayload(t *testing.T) {
	fset := token.NewFileSet()
	file, info, typeErrs, err := analyzeFile(fset, "testdata/leak_chanofchan.go")
	if err != nil {
		t.Fatalf("analyzeFile: %v", err)
	}
	if len(typeErrs) > 0 {
		t.Fatalf("type errors: %v", typeErrs)
	}

	leaks := CheckChannelConfinement(info, file)
	assertExactKinds(t, leaks, []LeakKind{LeakPayload})
}

// assertExactKinds checks leaks has exactly len(want) entries and that
// want's kinds are all present — order-independent, but a leak count that
// doesn't match want's length fails, so a rule that over- or under-fires
// (e.g. a container exemption that doesn't actually suppress anything)
// can't hide behind a "contains" check.
func assertExactKinds(t *testing.T, leaks []Leak, want []LeakKind) {
	t.Helper()
	if len(leaks) != len(want) {
		t.Fatalf("got %d leaks, want %d: %+v", len(leaks), len(want), leaks)
	}
	for _, k := range want {
		found := false
		for _, l := range leaks {
			if l.Kind == k {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected a leak of kind %v, got %+v", k, leaks)
		}
	}
}
