// The "nil out a channel to disable a select case" idiom bilc's own
// alt-guard lowering emits for `(cond) && chan -> var` — a bare channel
// copy that looks exactly like LeakAlias's shape, but isn't a leak: it's
// bilc's own generated code, and the copy only ever feeds the immediately
// following select. See confinement.go's nilGuardedAliases. Must NOT be
// flagged.
package main

func consume(in, request <-chan int, cond1, cond2 bool) {
	var v int

	bilGuard0 := in
	if !cond1 {
		bilGuard0 = nil
	}
	bilGuard1 := request
	if !cond2 {
		bilGuard1 = nil
	}

	select {
	case v = <-bilGuard0:
		println(v)
	case v = <-bilGuard1:
		println(v)
	}
}

func main() {
	in := make(chan int)
	request := make(chan int)
	consume(in, request, true, false)
}
