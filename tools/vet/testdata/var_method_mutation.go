// A package-level variable mutated via a pointer-receiver method call from
// one branch, read directly by another — the method call itself isn't a
// plain function call collectVarUsage's CallExpr case follows, and without
// treating it as a potential write, counter's mutation is invisible (the
// generic *ast.Ident visit records it as merely "used"). Must be flagged.
package main

import "sync"

type Counter struct{ n int }

func (c *Counter) Bump() { c.n++ }

var counter Counter

func bump() {
	counter.Bump()
}

func read() {
	println(counter.n)
}

func main() {
	par(
		func() { bump() },
		func() { read() },
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
