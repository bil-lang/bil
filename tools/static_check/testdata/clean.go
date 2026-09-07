// One sender proc, one receiver proc, sharing one channel — the expected,
// legal shape (one input use, one other output use). Must
// not be flagged.
package main

import "sync"

func sender(c chan<- int) {
	c <- 42
	c <- 99
}

func receiver(c <-chan int) {
	x := <-c
	println(x)
}

func main() {
	c := make(chan int)

	par(
		func() { sender(c) },
		func() { receiver(c) },
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
