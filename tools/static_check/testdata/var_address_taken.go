// &x taken in one branch, x used in another — variables.go's UnaryExpr/
// token.AND handling treats address-of as a write to the pointee, the
// generalized-past-call-shaped-branches version of bilc's own
// checkNoPointerParams. This pattern was previously only demonstrated ad
// hoc (a hand-broken example, never committed); this fixture locks it in.
// Must be flagged by CheckSharedVariables.
package main

import "sync"

func mutate(p *int) {
	*p = 5
}

func main() {
	x := 0

	par(
		func() { mutate(&x) },
		func() { println(x) },
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
