// A struct with a slice field, the same struct value passed by value into
// two branches' proc calls — before the deep inScopeVarType fix, this was
// wrongly treated as a safe value type (only the struct's own top-level
// kind was checked, not its fields). Copying the struct by value only
// copies its header; box.data's backing array is still genuinely shared.
// This fixture confirms the struct is now excluded (not silently trusted)
// — deferred to the same array/slice remainder as a bare slice, not a new
// "caught" diagnostic. Must NOT be flagged by CheckSharedVariables (it's
// out of scope, not verified safe).
package main

import "sync"

type box struct {
	data []int
}

func fill(b box) {
	b.data[0] = 1
}

func read(b box) {
	println(b.data[0])
}

func main() {
	b := box{data: []int{0}}

	par(
		func() { fill(b) },
		func() { read(b) },
	)
}

func par(branches ...func()) {
	var wg sync.WaitGroup
	wg.Add(len(branches))
	for _, br := range branches {
		go func(br func()) {
			defer wg.Done()
			br()
		}(br)
	}
	wg.Wait()
}
