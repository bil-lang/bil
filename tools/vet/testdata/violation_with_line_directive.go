// Same violation as violation_bare.go (two branches both receive from the
// same channel), but with a leading `//line` directive ahead of the first
// receive — exactly what bilc's own resync mechanism (bilc.go) now emits to
// map the transpiled Go it hands to Check back to the original .bil file
// and line. This fixture pins that Check itself needs no changes to honor
// it: go/parser (via analyzeFile's parser.ParseFile) applies `//line`
// directives unconditionally, so a violation's reported position already
// comes out named "/virtual/original.bil:99" here with no vet-side
// line-tracking at all. The directive's path is deliberately absolute:
// go/scanner resolves a *relative* //line filename against the directory
// of the file being parsed (this file, i.e. testdata/), not the caller's
// working directory — bilc itself has to pass an absolute .bil path into
// its own directives for exactly this reason, since the transpiled Go it
// hands to Check lives under a temp directory, not the .bil file's own.
package main

import "sync"

func main() {
	in := make(chan int)

	par(
		func() {
//line /virtual/original.bil:99
			x := <-in
			println(x)
		},
		func() {
			y := <-in
			println(y)
		},
	)
}

func par(branches ...func()) {
	var wg sync.WaitGroup
	wg.Add(len(branches))
	for _, b := range branches {
		go func(b func()) {
			defer wg.Done()
			b()
		}(b)
	}
	wg.Wait()
}
