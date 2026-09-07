// A channel embedded in a struct literal — bilc's own checkNoChannelAliasing
// (a bare `x := c` token scan) can't see this shape at all. Must be flagged
// (LeakComposite). The slice-of-channel case right below it must ALSO be
// flagged for its pre-existing channel c (channel arrays get the same
// confinement treatment as scalar channels, not a blanket exemption) — but
// its second element, a fresh make(chan int) call, must NOT be, matching
// the same "fresh factory result" exemption every other rule in this file
// already gives a direct make() call.
package main

type Box struct {
	ch chan int
}

func main() {
	c := make(chan int)
	b := Box{ch: c}
	_ = b

	cs := []chan int{c, make(chan int)}
	_ = cs
}
