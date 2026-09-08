// A channel copied into a second variable — an alias Bil channels have
// no legitimate way to form. Must be flagged (LeakAlias).
package main

func main() {
	c := make(chan int)
	c2 := c

	go func() {
		c2 <- 1
	}()
	<-c
}
