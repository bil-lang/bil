# Bil AI Cheatsheet

_Paste this file's contents alone into an AI chat prompt when asking it to write Bil — it's written to stand on its own, without the rest of the [Bil Guide](guide.md)._

**Bil is Go.** Every Go rule you already know — syntax, types, structs, generics, control flow, the standard library — applies unchanged. This section is the *complete* list of what's different. If it isn't listed here, write plain Go.

The `.bil` file extension is used; a file starts `package main` exactly like Go.

## The only new keywords

| Keyword | Meaning | Example |
|---|---|---|
| `par { A; B }` | Run `A` and `B` concurrently, wait for both (fork/join). | `par { sender(c); receiver(c) }` |
| `par i := range N { X }` | Spawn `N` copies of `X`, one per index `i` (replicated parallel). | `par i := range N { worker(in[i], out[i]) }` |
| `seq { ... }` | Marks a block as sequential. Pure readability sugar — Go already runs statements in order — used inside a `par` branch. | `seq { c <- 1; c <- 2 }` |
| `proc` | Alias for `func`, meaning "this is a process." Checked identically to `func`. | `proc worker(in <-chan int) { ... }` |
| `chan -> x` | **Receive, assign**, left-to-right: `c -> x` means `x = <-c`. `x` must already be declared — same rule everywhere this arrow appears (a bare statement, an `alt` guard, or a replicated `alt` guard). | `c -> x` |
| `chan :-> x` | **Receive, declare**: `c :-> x` means `x := <-c` — `x` is introduced fresh here, and must *not* already be declared. Same rule everywhere `-> ` does (mirrors Go's own `:=` for a channel receive; `->`/`:->` is Bil's own `=`/`:=` split, not occam's — occam has no inline declare-on-use at all). | `c :-> x` |
| `chan -> x, ok` | **Comma-ok, assign**: `c -> x, ok` means `x, ok = <-c` — Go's own two-value receive. `ok` is `false` exactly when `c` is closed and drained; both `x` and `ok` must already be declared. Either side may be `_`. | `c -> x, ok` |
| `chan :-> x, ok` | **Comma-ok, declare**: `c :-> x, ok` means `x, ok := <-c`, both fresh. Works in every context the single-value form does — a bare statement, an `alt` guard, or a replicated `alt` guard. `close(c)` is plain Go, not a Bil keyword; this is what lets you actually observe it. | `c :-> x, ok` |
| `chan -> Method(args)` | Receive on a call-shaped right side. | `time -> After(d)` |
| `alt { g1 { ... } g2 { ... } }` | Wait for whichever guard becomes ready first, then run that body (like `select`, but guards read `chan -> target { body }` / `chan :-> target { body }`, not `case x := <-chan:`). | see pattern 3 below |
| `(cond) && chan -> x { ... }` | A conditional guard inside `alt` — only eligible when `cond` is true. `-> `/`:->` both work here, same assign/declare rule. | `(len(q)>0) && req -> _ { ... }` |
| `alt i := range N { ... }` | Replicated `alt` — one process listening across a runtime-sized set of channels at once. The bind target follows the same `-> `/`:->` choice as any other receive — `i` (the replica index) is always fresh either way. | `alt i := range nClients { chans[i] :-> v { ... } }` |
| `pri alt { ... }` | Same as `alt`, but branches are tried in the order written — first ready one wins, even if a later one is also ready. Plain `alt` has **no** priority (picks pseudo-randomly among ready branches). | see pattern 3 |
| `skip` | No-op that succeeds immediately. As a block (`skip { ... }`), it's the default/non-blocking guard for an `alt`. | `skip { println("idle") }` |
| `stop` | Deliberate, permanent halt of the current process (never returns). | `stop` |

Helper generics (already provided, just call them — don't redefine): `makeChans[T](n)` makes `n` channels of type `T`; `splitN(slice, n)` splits a slice into `n` disjoint chunks for handing to replicated workers; `splitN2D(matrix, nr, nc)` does the 2D version.

## Hard rules — violating any of these is a compile-time error, not a style nit

1. **Channels are always unbuffered.** Never write `make(chan T, N)` with `N > 0` — always `make(chan T)`.
2. **There is no `go` keyword.** All concurrency goes through `par` / `par range`. Never write `go f()`.
3. A given `chan` may be written (`<-`) in exactly **one** branch of a `par` and read (`->`) in exactly **one other** branch.
4. A variable captured from an outer scope that's *written* in one `par` branch cannot be *read* in another branch.
5. A pointer, map, or interface value touched (read OR write) by more than one branch of the same `par` is rejected outright — even read-only sharing of these three types across branches is disallowed. (Plain value types, e.g. `int`/`struct` by value, are fine to read in multiple branches as long as none of them write it — rule 4.)
6. Slice/array elements may be written from more than one `par` branch only when each branch's index range is *provably disjoint* from the others — use `splitN`/`splitN2D` to get disjoint chunks rather than hand-slicing.
7. Exception to rule 3/5: a `time` channel (e.g. from `time.After`) may be read from multiple `par` branches.

## Canonical patterns

**1. Two processes, one channel**
```go
package main

func main() {
	c := make(chan int)
	par {
		seq {
			c <- 42
			c <- 99
		}
		seq {
			var x, y int
			c -> x
			c -> y
			println(x)
			println(y)
		}
	}
}
```

**2. Worker farm (replicated parallel)**
```go
package main

const n = 3

proc worker(in <-chan int, out chan<- int) {
	var x int
	in -> x
	out <- x * x
}

func main() {
	toWorker := makeChans[int](n)
	fmWorker := makeChans[int](n)

	par {
		par i := range n {
			worker(toWorker[i], fmWorker[i])
		}
		seq {
			for i := range n {
				toWorker[i] <- i + 1
			}
			for i := range n {
				println(<-fmWorker[i])
			}
		}
	}
}
```

**3. `alt` with a `skip` default (non-blocking poll)**
```go
package main

proc poller(c <-chan int) {
	var x int
	alt {
		c -> x {
			println(x)
		}
		skip {
			println("nothing ready")
		}
	}
}
```

**4. Tagged/variant channel protocol**
```go
package main

type Msg interface{ isMsg() }
type Info struct{ Code int }
type Warn struct{ Code int }

func (Info) isMsg() {}
func (Warn) isMsg() {}

proc logReceiver(in <-chan Msg) {
	switch v := (<-in).(type) {
	case Info:
		println("info", v.Code)
	case Warn:
		println("warn", v.Code)
	}
}
```

**5. Pipeline (chain of processes)**
```go
package main

const n = 10
const limit = 30

proc generator(out chan<- int) {
	for v := 2; v <= limit; v++ {
		out <- v
	}
	stop
}

proc filter(in <-chan int, out chan<- int) {
	var prime, v int
	in -> prime
	println(prime)
	for {
		in -> v
		if v%prime != 0 {
			out <- v
		}
	}
}

func main() {
	c := makeChans[int](n + 1)
	par {
		generator(c[0])
		par i := range n {
			filter(c[i], c[i+1])
		}
	}
}
```

## If you have shell access (agentic tools only)

Don't trust generated Bil blind — validate it: `bil vet file.bil` transpiles to Go and runs the static safety checker (all 7 rules above) without executing anything. It's fast and will immediately flag a buffered channel, a stray `go`, or cross-branch sharing. `bil run file.bil` does the same and then actually runs the program. Fix and re-run `bil vet` until it's clean before considering the code done.
