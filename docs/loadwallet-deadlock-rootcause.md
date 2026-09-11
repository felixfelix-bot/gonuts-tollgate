# Root-Cause Analysis: LoadWallet Deadlock (issue #1)

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

## Blocking call path (second LoadWallet)

```
wallet.LoadWallet()                              wallet/wallet.go:85
  └─ wallet.InitStorage(path)                    wallet/wallet.go:76
       └─ storage.InitBolt(path)                 wallet/storage/bolt.go:38
            └─ bolt.Open(..., nil)               bbolt db.go:248
                 └─ bbolt.flock(db, true, ...)   bbolt bolt_unix.go:44
                      └─ time.Sleep() loop       (indefinite — Timeout=0)
```

## Root cause

`storage.InitBolt()` passed `nil` as the `*bolt.Options` argument to
`bolt.Open()`. With `nil` options bbolt uses `DefaultOptions` where
`Timeout = 0`, meaning "wait indefinitely" for the file lock.

When the first `LoadWallet()` opens the DB and does not call `Close()`
before the second `LoadWallet()` runs on the same directory, the second
`bolt.Open()` enters an infinite `time.Sleep` retry loop inside
`flock()` and never returns.

## Goroutine dump evidence (pre-fix, from `go test -timeout 10s`)

```
goroutine 28 [sleep]:
time.Sleep(0x2faf080)
    /usr/lib/go-1.24/src/runtime/time.go:338 +0x165
go.etcd.io/bbolt.flock(0xc0004e08c0?, 0x1, 0x0)
    /home/c03rad0r/go/pkg/mod/go.etcd.io/bbolt@v1.4.0/bolt_unix.go:44 +0xd7
go.etcd.io/bbolt.Open({0xc0004e08c0, 0x45}, 0x180, 0x0)
    /home/c03rad0r/go/pkg/mod/go.etcd.io/bbolt@v1.4.0/db.go:248 +0x525
github.com/OpenTollGate/gonuts-tollgate/wallet/storage.InitBolt({0xc000166840?, 0x3b?})
    /home/c03rad0r/repos/gonuts-tollgate/wallet/storage/bolt.go:38 +0x65
github.com/OpenTollGate/gonuts-tollgate/wallet.InitStorage(...)
    /home/c03rad0r/repos/gonuts-tollgate/wallet/wallet.go:76
github.com/OpenTollGate/gonuts-tollgate/wallet.LoadWallet(...)
    /home/c03rad0r/repos/gonuts-tollgate/wallet/wallet.go:85 +0x9f
```

The blocked goroutine is in `time.Sleep` inside `bbolt.flock()` at
`bolt_unix.go:44`, called from `bolt.Open()` at `db.go:248`, called
from `storage.InitBolt()` at `bolt.go:38`, called from
`wallet.LoadWallet()` at `wallet.go:85`.

## Fix

Commit `60a7405` passes `&bolt.Options{Timeout: 5 * time.Second}` to
`bolt.Open()` so the second open fails with a timeout error after ~5s
instead of hanging forever.

```go
// wallet/storage/bolt.go:39
db, err := bolt.Open(filepath.Join(path, "wallet.db"), 0600,
    &bolt.Options{Timeout: 5 * time.Second})
```

## Regression test

`wallet/load_test.go` — `TestLoadWalletTwiceDoesNotDeadlock`

Calls `LoadWallet()` twice on the same directory. The second call runs
in a goroutine with a `select` on `time.After(10 * time.Second)`. The
test:
- **FAILS** (10s timeout) when the bug is present (no bolt timeout)
- **PASSES** when the fix is applied (second call returns error after
  ~5s bolt timeout)

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