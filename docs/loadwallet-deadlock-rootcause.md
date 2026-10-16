# Root-Cause Analysis: LoadWallet Deadlock (issue #1)

Status: **verified and reproduced** — see §"Independent re-verification" at the
bottom for the RED/GREEN evidence and the one correctness correction applied to
this note.

## Summary

`wallet.LoadWallet()` blocks forever when called twice on the same
wallet directory in one process. The second call deadlocks inside
`bolt.Open()` waiting on an `flock()` file lock that the first call
still holds.

## Exact lock primitive

**bbolt file lock (`flock`)** — `syscall.Flock(fd, LOCK_EX)` on Linux.

bbolt's `bolt.Open()` acquires an exclusive OS-level advisory file lock
on `wallet.db` via `flock(2)`. The lock is held for the entire lifetime
of the `*bolt.DB` handle and released on `db.Close()`.

The blocking primitive is not a Go `sync.Mutex`, `sync.Once` or channel —
it is an **OS advisory file lock** taken through `syscall.Flock`. The
"waiting" is a userspace retry loop, so the goroutine shows up as
`[sleep]` (not `[sync.Mutex.Lock]` / `[semacquire]`) in a goroutine dump.

## Blocking call path (second LoadWallet)

Pre-fix line numbers — verified against the pre-fix tree
(`wallet/storage/bolt.go` at its pre-fix revision, where `InitBolt` is at
L37 and the `bolt.Open(..., nil)` call is at **L38**):

```
wallet.LoadWallet()                              wallet/wallet.go:85
  └─ wallet.InitStorage(path)                    wallet/wallet.go:76
       └─ storage.InitBolt(path)                 wallet/storage/bolt.go:38  <- bolt.Open(..., nil)
            └─ bolt.Open(..., nil)               bbolt db.go:248
                 └─ bbolt.flock(db, true, ...)   bbolt bolt_unix.go:44
                      └─ time.Sleep() loop       (indefinite — Timeout=0)
```

`wallet/wallet.go` is byte-identical before and after the fix, so
`wallet.go:76` and `wallet.go:85` are correct in both states. In the
**fixed** tree `bolt.Open` moves to `wallet/storage/bolt.go:39` (the
`&bolt.Options{...}` argument replaces the `nil`).

## Root cause

`storage.InitBolt()` passed `nil` as the `*bolt.Options` argument to
`bolt.Open()`. With `nil` options bbolt uses `DefaultOptions` where
`Timeout = 0`, meaning "wait indefinitely" for the file lock.

bbolt's `flock()` (bolt_unix.go:34-48) makes this explicit:

```go
for {
    err := syscall.Flock(int(fd), flag)
    if err == nil {
        return nil
    } else if err != syscall.EWOULDBLOCK {
        return err
    }

    // If we timed out then return an error.
    if timeout != 0 && time.Since(t) > timeout-flockRetryTimeout {
        return errors.ErrTimeout
    }

    // Wait for a bit and try again.
    time.Sleep(flockRetryTimeout)
}
```

With `Timeout == 0` the `timeout != 0` guard is never entered, so the
loop can only exit by acquiring the lock. The `time.Sleep` in that loop
is the `[sleep]` frame in the goroutine dump.

When the first `LoadWallet()` opens the DB and does not call `Close()`
before the second `LoadWallet()` runs on the same directory, the second
`bolt.Open()` enters that infinite retry loop and never returns.

## Goroutine dump evidence (pre-fix, verbatim)

From `docs/loadwallet-deadlock-goroutine-dump-prefix.log` — the raw
`SIGQUIT` dump of the test binary linked against **byte-exact pre-fix**
`wallet/storage/bolt.go` (sha256
`2efbfa477455dc4f887af12f6938af88074b54b3598842d95f0351dfc58b0e8e`,
i.e. `bolt.Open(..., 0600, nil)` at L38):

```
goroutine 22 gp=0xc00024c000 m=nil [sleep]:
runtime.gopark(...)
	/usr/lib/go-1.24/src/runtime/proc.go:435 +0xce
time.Sleep(0x2faf080)
	/usr/lib/go-1.24/src/runtime/time.go:338 +0x165
go.etcd.io/bbolt.flock(0xc000144930?, 0x1, 0x0)
	/home/c03rad0r/go/pkg/mod/go.etcd.io/bbolt@v1.4.0/bolt_unix.go:44 +0xd7
go.etcd.io/bbolt.Open({0xc000144930, 0x62}, 0x180, 0x0)
	/home/c03rad0r/go/pkg/mod/go.etcd.io/bbolt@v1.4.0/db.go:248 +0x525
github.com/OpenTollGate/gonuts-tollgate/wallet/storage.InitBolt({0xc000032240?, 0x58?})
	/home/c03rad0r/worktrees/gnt-loaddeadlock/wallet/storage/bolt.go:38 +0x65
github.com/OpenTollGate/gonuts-tollgate/wallet.InitStorage(...)
	/home/c03rad0r/worktrees/gnt-loaddeadlock/wallet/wallet.go:76
github.com/OpenTollGate/gonuts-tollgate/wallet.LoadWallet(...)
	/home/c03rad0r/worktrees/gnt-loaddeadlock/wallet/wallet.go:85 +0x9f
github.com/OpenTollGate/gonuts-tollgate/wallet.TestLoadWalletTwiceDoesNotDeadlock.func1()
	/home/c03rad0r/worktrees/gnt-loaddeadlock/wallet/load_test.go:121
```

The blocked goroutine is in `time.Sleep` inside `bbolt.flock()` at
`bolt_unix.go:44`, called from `bolt.Open()` at `db.go:248`, called
from `storage.InitBolt()` at `bolt.go:38`, called from
`wallet.LoadWallet()` at `wallet.go:85`.

Note the argument `0x0` (the `timeout`) passed to `bbolt.Open` and on to
`bbolt.flock` — that zero is the `DefaultOptions.Timeout` and is the
whole bug.

The same dump is also obtainable *without* an external `-timeout`:
`wallet/load_test.go` now allocates a buffer and calls
`runtime.Stack(buf, true)` inside its own guard, so the blocked goroutine
is printed directly in the test failure output. See
`docs/loadwallet-deadlock-red-intest-dump.log` (FAIL after 62.21s, guard
60s) for that capture.

## Fix

Revision `60a7405` passes `&bolt.Options{Timeout: 5 * time.Second}` to
`bolt.Open()` so the second open fails with a timeout error after ~5s
instead of hanging forever.

```go
// wallet/storage/bolt.go:39 (fixed state)
db, err := bolt.Open(filepath.Join(path, "wallet.db"), 0600,
    &bolt.Options{Timeout: 5 * time.Second})
```

Upstream has since hardened this further: on the current upstream tip
`InitBolt` uses a named `boltOpenTimeout` constant and maps
`bolt.ErrTimeout` to an exported `storage.ErrDBLocked` sentinel so
callers can branch on the condition instead of string-matching the
error.

## Regression test

`wallet/load_test.go` — `TestLoadWalletTwiceDoesNotDeadlock`

Calls `LoadWallet()` twice on the same directory. The second call runs
in a goroutine with a `select` on `time.After(deadlockGuard)`. The
test:

- **FAILS** (guard expiry) when the bug is present (no bolt timeout),
  and prints a full goroutine dump naming the blocked primitive
- **PASSES** when the fix is applied (second call returns error after
  ~5s bolt timeout)

Recommended invocation — the outer `-timeout` must be **larger than the
60s guard** so the in-test diagnostic fires first:

```
go test -timeout 90s -run TestLoadWalletTwiceDoesNotDeadlock ./wallet/
```

In the pre-fix state expect `--- FAIL ... (62.21s)`. In the fixed state
expect `--- PASS` in ~5-9s (the bolt lock timeout dominates).

## Notes for the fix worker

1. The 5s timeout is a mitigation, not a complete fix. The caller of
   `LoadWallet()` still gets an error on the second call. If the
   use-case requires two concurrent wallet handles on the same
   directory, the architecture needs to change (e.g. share a single
   DB handle, or serialize open/close).

2. `LoadWallet()` has a `defer` that calls `db.Close()` only on error
   (`isErr == true`). On success the DB stays open for the wallet's
   lifetime via `w.db`. The only way to release the lock is
   `Wallet.Shutdown()` → `db.Close()`.

3. The storage-level test `TestInitBoltLockTimeout` (in
   `wallet/storage/bolt_test.go`) tests the same primitive at the
   storage layer. The wallet-level test
   `TestLoadWalletTwiceDoesNotDeadlock` tests the full `LoadWallet`
   call path end-to-end.

## Independent re-verification (this pass)

Method — three states checked, each with real tool output:

| State | `wallet/storage/bolt.go` | Result |
|---|---|---|
| pre-fix `nil` options | `...0600, nil)` @ L38 | **FAIL** 62.21s, dump shows `[sleep]` in `bbolt.flock` |
| fixed `Timeout: 5s` | `...&bolt.Options{...})` @ L39 | **PASS** 5.3s / 5.6s / 6.2s / 6.7s / 8.4s |

- RED used the **byte-exact pre-fix file** restored from revision
  `4b7d125` (`git show 4b7d125:wallet/storage/bolt.go`), NOT a
  hand-edited approximation, so the line numbers in the dump are the
  real pre-fix lines.
- GREEN runs are reproducible: 5 independent runs, all PASS, wall time
  5.30s–8.35s. The `bolt.Options{Timeout: 5 * time.Second}` value is
  what shows up as the observed ~5s floor.
- The `22.63s` green outlier seen once was **machine load**, not the
  repository: a simultaneous parallel `go build` of an unrelated tree was
  competing for CPU. This is why the guard in the test was raised from
  `10 * time.Second` to a named `deadlockGuard = 60 * time.Second` in
  this pass — the old 10s guard was within ~1.6x of an observed
  legitimate green time and would have produced a **false deadlock
  report on a loaded machine**. A real deadlock blocks forever, so
  raising the guard costs nothing in detection power.

### Correction applied to this note

The original version of this note cited `bolt.go:38` for the
`bolt.Open` call in the **fixed** tree. That is off by one: the fix adds
a `time` import, shifting `bolt.Open` to L39 in the fixed tree; L38 is
the pre-fix line. Both numbers are now stated explicitly with the tree
they belong to.

### Evidence files (committed)

| File | Contents |
|---|---|
| `docs/loadwallet-deadlock-goroutine-dump-prefix.log` | raw SIGQUIT dump, pre-fix binary |
| `docs/loadwallet-deadlock-red-intest-dump.log` | in-test `runtime.Stack` dump, pre-fix, FAIL 62.21s |
| `docs/loadwallet-deadlock-green-run.log` | fixed-state PASS output |

Note: the repository `.gitignore` contains `*.txt`, so these captures use
the `.log` extension in order to be tracked.
