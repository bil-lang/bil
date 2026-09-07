// A package-level channel sent on directly by procs called from two
// different branches — neither receives it as a parameter, so channels.go's
// existing parameter-substitution path never sees it; only recursing into
// both calls and checking isFree at that depth (which reduces to
// "package-level") finds the conflict. Must be flagged as two branches
// both sending on the same channel.
package main

import "sync"

var ch = make(chan int)

func sendA() {
	ch <- 1
}

func sendB() {
	ch <- 2
}

func main() {
	par(
		func() { sendA() },
		func() { sendB() },
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
