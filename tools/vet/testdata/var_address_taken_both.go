// &x taken in *both* branches — the symmetric case bilc's own
// checkNoPointerParams doc comment called out specifically ("a variable
// touched by pointer in one branch may not be touched at all — by pointer
// or by value — in any other branch"), distinct from var_address_taken.go's
// one-pointer/one-value shape. Must be flagged (both directions, since
// variables.go's UnaryExpr/token.AND case marks x used as well as written
// in each branch).
package main

import "sync"

func mutate(p *int) {
	*p = 5
}

func main() {
	x := 0

	par(
		func() { mutate(&x) },
		func() { mutate(&x) },
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
