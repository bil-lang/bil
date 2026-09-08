// A function handing back a channel it merely received, versus one that
// manufactures and returns a fresh channel. Only the first is a leak
// (LeakReturn) — a factory that returns make(chan T) is a legitimate
// creation, not an alias of something already bound.
package main

func passThrough(c chan int) chan int {
	return c
}

func factory() chan int {
	return make(chan int)
}

func main() {
	c := make(chan int)
	_ = passThrough(c)
	_ = factory()
}
