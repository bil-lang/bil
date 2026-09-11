# Bil AI Cheatsheet — occam-grounded variant

_Paste this file's contents alone into an AI chat prompt when asking it to write Bil — it's written to stand on its own. See also [`ai-cheatsheet.md`](ai-cheatsheet.md), a Go-framed variant of the same material._

Bil's *base* language is Go — types, structs, generics, control flow, the standard library, all unchanged. But Bil's new keywords aren't invented: they're occam's own `PAR`/`SEQ`/`ALT`/`PROC`/`SKIP`/`STOP` vocabulary, kept almost verbatim (just lowercased, and `PRI ALT` kept as its own two tokens `pri alt`). If you already know occam, or CSP more generally, you already know Bil's concurrency model — map the keyword, adjust the punctuation to Go's, done. This cheatsheet pairs each new keyword, and each worked pattern, with its occam original for exactly that reason. The 7 hard rules below aren't new invented restrictions either — they're occam's own compiler-enforced variable/channel usage rules (occam 2 Reference Manual, Appendix E: a free variable written in one process can't be read in another, a channel is unidirectional between exactly two processes, and so on), which Bil's static checker (`bil vet`) re-derives and enforces on top of Go.

The `.bil` file extension is used; a file starts `package main` exactly like Go.

## The only new keywords

| Keyword | occam | Meaning |
|---|---|---|
| `par { A; B }` | `PAR` | Run `A` and `B` concurrently, wait for both (fork/join). |
| `par i := range N { X }` | `PAR i = 0 FOR N` | Spawn `N` copies of `X`, one per index `i` (replicated parallel). |
| `seq { ... }` | `SEQ` | Marks a block as sequential. Pure readability sugar in Bil (Go already runs statements in order); in occam `SEQ` is load-bearing since `PAR` is the default otherwise implied. |
| `proc` | `PROC` | Declares a named process. Alias for `func`, checked identically. |
| `chan -> x` | `chan ? x` | **Receive, assign**, left-to-right just like occam: `c -> x` means `x = <-c`. `x` must already be declared, exactly as occam requires — and this is true everywhere `->` appears: a bare statement, an `alt` guard, or a replicated `alt` guard. |
| `chan :-> x` | _(no occam equivalent)_ | **Receive, declare**: `c :-> x` means `x := <-c` — `x` is introduced fresh here, and must *not* already be declared. A deliberate Go-idiomatic extension beyond occam (which has no inline declare-on-use at all, ever) — mirrors Go's own `:=` for a channel receive (`x := <-c`, `case v := <-ch:`). Same rule everywhere `->` does: pick `->` or `:->` per receive, independent of which construct surrounds it. |
| `chan -> x, ok` | _(no occam equivalent)_ | **Comma-ok, assign**: `c -> x, ok` means `x, ok = <-c` — Go's own two-value receive, with no occam analog (occam channels have no "closed" state to detect). `ok` is `false` exactly when `c` is closed and drained; `x`/`ok` must already be declared. Either side may be `_`. |
| `chan :-> x, ok` | _(no occam equivalent)_ | **Comma-ok, declare**: `c :-> x, ok` means `x, ok := <-c`, both fresh. Works in every context the single-value form does — bare statement, `alt` guard, replicated `alt` guard. `close(c)` is plain Go, not a Bil keyword; this is what lets you actually observe it. |
| `chan -> Method(args)` | _(no occam equivalent)_ | Receive on a call-shaped right side — a Go-interop escape hatch, needed for things like `time -> After(d)` that have no occam analog. |
| `chan -> case v { T1 { ... } T2 { ... } }` | `chan ? CASE tag1; p1 { ... } tag2; p2 { ... }` | Receive-and-dispatch on a tagged/variant payload (occam `PROTOCOL ... CASE`). Unlike every other receive, this one always declares `v` fresh — Go's type-switch has no assignment form, so `->`/`:->` doesn't apply here (occam's own `CASE` predeclares `p1`/`p2` like anything else in the language; this is the one construct where Bil can't match that). |
| `alt { g1 { ... } g2 { ... } }` | `ALT` | Wait for whichever guard becomes ready first, then run that body. Guards read left-to-right (`chan -> target { body }` or `chan :-> target { body }`), echoing occam's `chan ? x`. |
| `(cond) && chan -> x { ... }` | `(cond) & chan ? x` | A conditional guard inside `alt` — only eligible when `cond` is true. (`&&` matches Go's own logical-and token, not occam's bare `&`.) `->`/`:->` both work here, same assign/declare rule. |
| `alt i := range N { ... }` | `ALT i = 0 FOR N` | Replicated `alt` — one process listening across a runtime-sized set of channels at once. The bind target takes the same `->`/`:->` choice as any other receive; `i` (the replica index) is fresh either way, since it's the construct's own runtime dispatch result, not something occam's static replicator has an equivalent of. |
| `pri alt { ... }` | `PRI ALT` | Same as `alt`, but branches are tried in priority order — the first ready one wins, even if a lower-priority one is also ready. |
| `skip` | `SKIP` | No-op that succeeds immediately. As a block (`skip { ... }`), it's also the default/non-blocking guard for an `alt` — same role occam's bare `SKIP` guard plays. |
| `stop` | `STOP` | Deliberate, permanent halt of the current process (never returns). |

Helper generics (already provided, just call them — don't redefine): `makeChans[T](n)` makes `n` channels of type `T`, the equivalent of declaring `[n]CHAN OF T`; `splitN(slice, n)` splits a slice into `n` disjoint chunks for handing to replicated workers; `splitN2D(matrix, nr, nc)` does the 2D version. None of these three have a direct occam keyword — occam programs did the equivalent by hand, per-example.

## Hard rules — violating any of these is a compile-time error, not a style nit

1. **Channels are always unbuffered.** Never write `make(chan T, N)` with `N > 0` — always `make(chan T)`. Matches occam's `CHAN OF T`, which has no buffering concept at all.
2. **There is no `go` keyword.** All concurrency goes through `par` / `par range` — occam has no equivalent of a dynamically-spawned goroutine either, only `PAR`.
3. A given `chan` may be written (`<-`) in exactly **one** branch of a `par` and read (`->`) in exactly **one other** branch — occam's channel usage rule.
4. A variable captured from an outer scope that's *written* in one `par` branch cannot be *read* in another branch — occam's free-variable usage rule.
5. A pointer, map, or interface value touched (read OR write) by more than one branch of the same `par` is rejected outright — even read-only sharing of these three types across branches is disallowed. (Plain value types, e.g. `int`/`struct` by value, are fine to read in multiple branches as long as none of them write it — rule 4. occam has no pointers/maps/interfaces at all, so this rule is Bil's own extension of the same underlying principle to Go's reference types.)
6. Slice/array elements may be written from more than one `par` branch only when each branch's index range is *provably disjoint* from the others — use `splitN`/`splitN2D` to get disjoint chunks rather than hand-slicing. occam's array usage rule, same idea.
7. Exception to rule 3/5: a `time` channel (e.g. from `time.After`) may be read from multiple `par` branches — a Bil-specific carve-out, since occam has no `time` package to reason about.

## Canonical patterns (occam alongside Bil)

**1. Two processes, one channel**

occam:
```occam
CHAN OF INT c:

PAR
  SEQ
    c ! 42
    c ! 99

  SEQ
    INT x:
    INT y:

    c ? x
    c ? y

    out.int(x, 0)
    out.int(y, 0)
```

Bil:
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

occam:
```occam
VAL INT N IS 3:

PROC worker(CHAN OF INT in, CHAN OF INT out)
  INT x:
  SEQ
    in ? x
    out ! (x * x)
:

[N]CHAN OF INT toWorker:
[N]CHAN OF INT fmWorker:
INT r:

PAR
  PAR i = 0 FOR N
    worker(toWorker[i], fmWorker[i])

  SEQ
    SEQ i = 0 FOR N
      toWorker[i] ! (i + 1)
    SEQ i = 0 FOR N
      SEQ
        fmWorker[i] ? r
        out.int(r, 0)
```

Bil:
```go
package main

const N = 3

proc worker(in <-chan int, out chan<- int) {
	var x int
	in -> x
	out <- x * x
}

func main() {
	toWorker := makeChans[int](N)
	fmWorker := makeChans[int](N)

	par {
		par i := range N {
			worker(toWorker[i], fmWorker[i])
		}
		seq {
			for i := range N {
				toWorker[i] <- i + 1
			}
			for i := range N {
				println(<-fmWorker[i])
			}
		}
	}
}
```

**3. `alt` with a `skip` default, plus `stop` (non-blocking poll)**

occam:
```occam
PROC poller(CHAN OF INT c)
  INT x:
  SEQ
    ALT
      c ? x
        IF
          x >= 0
            out.int(x, 0)
          TRUE
            STOP

      SKIP
        SKIP
:
```

Bil:
```go
package main

import "time"

proc poller(c <-chan int) {
	var x int
	for {
		alt {
			c -> x {
				if x >= 0 {
					println(x)
				} else {
					stop
				}
			}
			skip {
				println("skip")
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

proc sender(c chan<- int) {
	time.Sleep(150 * time.Millisecond)
	c <- 42
	time.Sleep(150 * time.Millisecond)
	c <- -1
}

func main() {
	c := make(chan int)

	par {
		poller(c)
		sender(c)
	}
}
```

**4. Tagged/variant channel protocol (`-> case`)**

occam:
```occam
PROTOCOL LOGMSG
  CASE
    info; INT
    warn; INT
:

PROC logSender(CHAN OF LOGMSG out)
  SEQ
    out ! info; 1
    out ! warn; 2
    out ! info; 3
:

PROC logReceiver(CHAN OF LOGMSG in)
  INT code:
  SEQ i = 0 FOR 3
    in ? CASE
      info; code
        out.string("info*n", 0)
      warn; code
        out.string("warn*n", 0)
:
```

Bil:
```go
package main

type LogMsg interface {
	isLogMsg()
}

type Info struct {
	Code int
}

func (Info) isLogMsg() {}

type Warn struct {
	Code int
}

func (Warn) isLogMsg() {}

proc logSender(out chan<- LogMsg) {
	out <- Info{Code: 1}
	out <- Warn{Code: 2}
	out <- Info{Code: 3}
}

proc logReceiver(in <-chan LogMsg) {
	for range 3 {
		in -> case v {
			Info {
				println("info", v.Code)
			}
			Warn {
				println("warn", v.Code)
			}
		}
	}
}

func main() {
	logs := make(chan LogMsg)
	par {
		logSender(logs)
		logReceiver(logs)
	}
}
```

**5. Pipeline (chain of processes)**

occam:
```occam
VAL INT N IS 10:
VAL INT LIMIT IS 30:

PROC generator(CHAN OF INT out)
  INT v:
  SEQ
    v := 2
    WHILE v <= LIMIT
      SEQ
        out ! v
        v := v + 1
    STOP
:

PROC filter(CHAN OF INT in, CHAN OF INT out)
  INT prime:
  INT v:
  SEQ
    in ? prime
    out.int(prime, 0)
    WHILE TRUE
      SEQ
        in ? v
        IF
          (v REM prime) <> 0
            out ! v
          TRUE
            SKIP
:

[N + 1]CHAN OF INT c:

PAR
  generator(c[0])
  PAR i = 0 FOR N
    filter(c[i], c[i + 1])
```

Bil:
```go
package main

const N = 10
const limit = 30

proc generator(out chan<- int) {
	for v := 2; v <= limit; v++ {
		out <- v
	}
	stop
}

proc filter(in <-chan int, out chan<- int) {
	var prime, v int
	seq {
		in -> prime
		println(prime)
		for {
			in -> v
			if v%prime != 0 {
				out <- v
			}
		}
	}
}

func main() {
	c := makeChans[int](N + 1)

	par {
		generator(c[0])
		par i := range N {
			filter(c[i], c[i+1])
		}
	}
}
```

## If you have shell access (agentic tools only)

Don't trust generated Bil blind — validate it: `bil vet file.bil` transpiles to Go and runs the static safety checker (all 7 rules above) without executing anything. It's fast and will immediately flag a buffered channel, a stray `go`, or cross-branch sharing. `bil run file.bil` does the same and then actually runs the program. Fix and re-run `bil vet` until it's clean before considering the code done.
