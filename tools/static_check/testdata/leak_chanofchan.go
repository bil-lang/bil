// A channel of channels, with an existing channel sent as a payload on it —
// exercises both LeakChanOfChan (the type declaration itself) and
// LeakPayload (the send) from one fixture.
package main

func main() {
	inner := make(chan int)
	outer := make(chan chan int)

	outer <- inner
}
