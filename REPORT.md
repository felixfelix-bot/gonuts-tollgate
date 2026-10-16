# REPORT — GNT-LOADDEADLOCK-1

Reproduce `wallet.LoadWallet()` double-call deadlock (issue
c03rad0r/gonuts-tollgate#1) with a deterministic failing regression test and
root-cause evidence naming the exact lock primitive and call path.

**Verdict: both acceptance criteria met, verified, committed, pushed.**
One material finding for the manager: the deliverable is a **duplicate** of
work already merged upstream, and the branch should **not** be PR'd against
`origin/main` as-is.

---

## 0. Deliverable

| Item | Value |
|---|---|
| Worktree | `/home/c03rad0r/worktrees/gnt-loaddeadlock` |
| Branch | `worker-base/GNT-LOADDEADLOCK-1` |
| HEAD | `31cc8367584c150df527ec635a3d997e9f3b6ced` |
| Pushed to | `fork-bot` = https://github.com/felixfelix-bot/gonuts-tollgate.git |
| Remote ref | `refs/heads/worker-base/GNT-LOADDEADLOCK-1` == `31cc836` (verified via `git ls-remote`) |
| Working tree | clean (`git status --porcelain` empty) |
| Commits | `60a7405` (fix, pre-existing), `934ce46` (test+note, pre-existing), `dd04e02`, `31cc836` |

Push is verified reproducible: `git ls-remote fork-bot <branch>` returned
`31cc8367584c150df527ec635a3d997e9f3b6ced`, identical to local `HEAD`.

Commits in this pass:

- `dd04e02` — test(wallet): harden LoadWallet deadlock regression test + verify root cause
  (6 files, +688/-31)
- `31cc836` — docs(wallet): record upstream reconciliation + full pass log

---

## 1. Acceptance criterion 1 — regression test committed

`wallet/load_test.go` :: `TestLoadWalletTwiceDoesNotDeadlock` — committed in
`934ce46`, **hardened in `dd04e02`**.

What it does: calls `LoadWallet(cfg)` once (holding an exclusive `flock` on
`wallet.db`), deliberately does **not** `Shutdown()` it, then calls
`LoadWallet(cfg)` again in a goroutine with a `select` on
`time.After(deadlockGuard)`. A `httptest` mock mint serves `/v1/keys` and
`/v1/keysets` so the happy path needs no network.

### Final committed-state run (exact task contract)

```
$ go test -timeout 90s -run TestLoadWalletTwiceDoesNotDeadlock -v -count=1 ./wallet/
=== RUN   TestLoadWalletTwiceDoesNotDeadlock
    load_test.go:113: mock mint at http://127.0.0.1:38541, keyset id=0090de6a40474267
    load_test.go:156: second LoadWallet returned error (expected when lock is held): InitStorage: error setting bolt db: timeout
--- PASS: TestLoadWalletTwiceDoesNotDeadlock (5.59s)
PASS
ok  github.com/OpenTollGate/gonuts-tollgate/wallet  5.602s
```

### RED proof — the test really does fail when the bug is present

The test was run against the **byte-exact pre-fix** `wallet/storage/bolt.go`
restored from revision `4b7d125` — not a hand-edited approximation — so the
line numbers in the dump are the true pre-fix lines.

Reconstructed via `git show 4b7d125:wallet/storage/bolt.go > wallet/storage/bolt.go`,
sha256 `2efbfa477455dc4f887af12f6938af88074b54b3598842d95f0351dfc58b0e8e`,
containing `bolt.Open(..., 0600, nil)` at **L38**.

In-test guard (the new mechanism):

```
load_test.go:173: LoadWallet deadlocked on second call: blocked on bbolt file lock
                  (flock) for >1m0s — see goroutine dump for
                  bolt.Open in wallet/storage/bolt.go:InitBolt (keyset=0090de6a40474267)
--- FAIL: TestLoadWalletTwiceDoesNotDeadlock (62.21s)
FAIL
```

External SIGQUIT of the pre-fix binary (independent mechanism, same result):

```
goroutine 22 [...] [sleep]:
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

Note the `0x0` timeout threaded into `bbolt.Open` and on to `bbolt.flock` —
that zero **is** the bug.

### GREEN repeatability

5 independent runs of the fixed tree, all PASS:
`5.30s / 5.62s / 6.17s / 6.68s / 8.35s`, plus a final 5.59s run above.
The ~5s floor is exactly the `bolt.Options{Timeout: 5 * time.Second}` value.

---

## 2. Acceptance criterion 2 — root-cause note naming lock primitive + file:line

`docs/loadwallet-deadlock-rootcause.md` — committed, and **corrected +
extended** in `dd04e02`.

### Exact lock primitive

An **OS-level advisory file lock** — `syscall.Flock(fd, LOCK_EX)` on Linux,
taken by bbolt's `bolt.Open()` on `wallet.db`. It is deliberately *not* a Go
`sync.Mutex`, `sync.Once`, or channel:

- the lock is an **fd-scoped `flock(2)`** held for the entire lifetime of the
  `*bolt.DB` handle and released only by `db.Close()`;
- the "waiting" is a **userspace retry loop**, which is why the blocked
  goroutine reports `[sleep]` rather than `[semacquire]` / `[sync.Mutex.Lock]`.

bbolt v1.4.0 `bolt_unix.go:34-48` makes the hang explicit:

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

With `Timeout == 0` (bbolt `DefaultOptions`, used because `InitBolt` passed
`nil` options) the `timeout != 0` guard is **never entered**, so the loop can
only ever exit by acquiring the lock. The `time.Sleep` on the last line is the
`[sleep]` frame in the dump.

### Call path, with correct line numbers for both trees

```
wallet.LoadWallet()                              wallet/wallet.go:85
  └─ wallet.InitStorage(path)                    wallet/wallet.go:76
       └─ storage.InitBolt(path)                 wallet/storage/bolt.go:38  (PRE-FIX)
            └─ bolt.Open(..., 0600, nil)         bbolt db.go:248
                 └─ bbolt.flock(db, true, 0)     bbolt bolt_unix.go:44
                      └─ time.Sleep() loop       (indefinite — Timeout=0)
```

`wallet/wallet.go` is byte-identical pre- and post-fix
(`git diff 4b7d125 HEAD -- wallet/wallet.go` → 0 lines), so `wallet.go:76`
and `wallet.go:85` are valid in both states.

### Correction applied to the pre-existing note

The original note cited `bolt.go:38` for the **fixed** tree. That is off by
one: the fix adds a `time` import, shifting `bolt.Open` to **L39** in the
fixed tree; L38 is the pre-fix line. Both are now stated with the tree they
belong to.

### Doc defect worth flagging to whoever wrote the original note

The note's call-path block claimed the goroutine would be dumped by
`go test -timeout 10s`. That is a **race**: the test's own guard was
`10 * time.Second`, so whether the in-test `t.Fatal` diagnostic or the Go
runtime's timeout dump won depended on scheduling. The hardened test removes
the race by calling `runtime.Stack(buf, true)` **in-test**, so the dump is
deterministic and no outer `-timeout` tuning is required.

---

## 3. Material finding — the 10s guard was flaky (real defect, fixed)

The original guard was `10 * time.Second`. Measured GREEN wall times of the
fixed tree: `5.30s, 5.62s, 6.17s, 6.68s, 8.35s`, plus **one 22.63s outlier**
observed while an unrelated parallel `go build` saturated the CPU.

A 10s guard is therefore within ~1.6x of a *legitimate* green run — on a
loaded machine or a slow CI runner this test would report a **false deadlock**
and block an innocent PR. That directly undermines the task's own goal of a
*deterministic* reproduction.

Fix applied in `dd04e02`:

- guard raised to a named `const deadlockGuard = 60 * time.Second`;
- the guard is now self-contained — it calls `runtime.Stack(buf, true)` and
  embeds the dump in the failure message, instead of relying on an outer
  `-timeout` firing first;
- `fmt` import dropped, `runtime` added; `gofmt -l` clean.

This costs nothing in detection power: a genuine deadlock blocks **forever**
(infinite retry loop), so any finite guard still catches it. The comment in
the test records the reasoning and the observed numbers.

---

## 4. Verification performed

| Check | Command | Result |
|---|---|---|
| Targeted test, final committed state | `go test -timeout 90s -run TestLoadWalletTwiceDoesNotDeadlock ./wallet/` | **PASS** 5.59s |
| RED proof, byte-exact pre-fix file | pre-fix binary + in-test guard | **FAIL** 62.21s + dump |
| RED proof, independent mechanism | SIGQUIT pre-fix binary | `[sleep]` in `bbolt.flock` |
| Full wallet + storage suites | `go test -count=1 ./wallet/ ./wallet/storage/` | `ok 17.100s` / `ok 19.528s` |
| Build of touched trees | `go build ./wallet/... ./crypto/... ./cashu/...` | exit 0 |
| vet | `go vet ./wallet/` | VET_OK |
| gofmt | `gofmt -l wallet/load_test.go` | clean (empty) |
| Push verified | `git ls-remote fork-bot <branch>` | `31cc836` == local HEAD |
| Tree clean | `git status --porcelain` | empty |

### Honest limitation: `go build ./...` does not complete

`go build ./...` (whole repo, including `mint/`) exits **124** (timeout) in
this environment. Cause: it wants uncached modules (`jackc/pgpassfile`,
`docker/cli`, `modernc.org/libc`, …) and there is no working network path —
`proxy.golang.org` DNS lookup times out on `127.0.0.53`. All local
verification therefore ran with `GOPROXY=off`.

This is a **pre-existing environment limitation, not a regression**: the
change in this pass touches only `wallet/load_test.go` (test) and two `docs/`
files, and every package this deliverable actually touches builds and tests
clean. I did not and cannot claim a whole-repo build.

---

## 5. Duplicate-dispatch finding (needs a manager decision)

**This deliverable duplicates work already merged upstream, in two forms.**

The branch base (`main` @ `934ce46`) is *ahead 2, behind 23* of `origin/main`.
Upstream is 23 commits ahead and already contains:

| | this branch | upstream `origin/main` |
|---|---|---|
| Fix | inline `&bolt.Options{Timeout: 5 * time.Second}` (`60a7405`) | named `boltOpenTimeout` const + maps `bolt.ErrTimeout` to exported **`storage.ErrDBLocked`** sentinel (`wallet/storage/bolt.go:46-50, 177-181`) |
| Wallet-level test | `TestLoadWalletTwiceDoesNotDeadlock` (`wallet/load_test.go`) | `TestLoadWallet_SecondInProcessCallWithoutShutdownDoesNotBlock` (`wallet/wallet_test.go:402-443`) |
| Guard | 60s, in-test `runtime.Stack` | 3 min, `runtime.Stack` |

`4b7d125` **is** an ancestor of `origin/main`
(`git merge-base --is-ancestor 4b7d125 origin/main` → YES), so upstream and
this branch share the pre-fix base; the divergence is purely fix
implementation. Upstream's fix is strictly better — a typed sentinel beats
string-matching a timeout error.

Recommendation (recorded in the root-cause note §"Reconciliation with
upstream"):

1. **Do not open this branch as a PR against `origin/main`.** It would
   re-apply the weaker inline fix on top of merged hardening and add a
   redundant second deadlock test.
2. The branch's remaining value is as the **byte-exact RED reproduction and
   goroutine dump** that the upstream commit does not carry.
3. If an *additional independent* regression test on the hardened upstream
   fix is actually wanted, rebase onto `origin/main` and reconcile with
   `TestLoadWallet_SecondInProcessCallWithoutShutdownDoesNotBlock` — keeping
   both means two tests that each hold an exclusive flock for ~5s.

I deliberately did **not** resolve item 3 unilaterally (no force-push, no
rewrite of upstream, no PR). `origin/main` is untouched, and `origin` is
READ-permission-only anyway.

---

## 6. Remaining steps (none required for this task's acceptance criteria)

Both acceptance criteria are met and pushed. Open, manager-gated:

1. Decide on §5 item 3 — rebase the hardened test onto `origin/main`, or close
   this as a duplicate now that upstream carries both a fix and a test.
2. If merging is wanted, the destination is a fork PR; `origin` is pull-only
   for this credential, so a maintainer must merge.

No work is left uncommitted or unpushed in the worktree.

---

## 7. Evidence files (committed, all under `docs/`)

| File | Contents |
|---|---|
| `docs/loadwallet-deadlock-rootcause.md` | root-cause note (corrected + reconciliation) |
| `docs/loadwallet-deadlock-goroutine-dump-prefix.log` | raw SIGQUIT dump, byte-exact pre-fix binary |
| `docs/loadwallet-deadlock-red-intest-dump.log` | in-test `runtime.Stack` dump, pre-fix, FAIL 62.21s |
| `docs/loadwallet-deadlock-green-run.log` | fixed-state PASS runs |
| `PROGRESS.md` | pass-by-pass log (crash-recovery map) |
| `REPORT.md` | this file |

`.gitignore` contains `*.txt`, so captures use the `.log` extension to be
trackable.

---

## 8. Ad-hoc verification pass (post-report)

Not a canonical suite run — this repo has no single canonical test/lint/build
command, and `go build ./...` cannot complete offline (§4). A focused
throwaway script was run under
`/home/c03rad0r/.cache/hermes/tmp/hermes-verify-*.sh` (created via `mktemp`,
removed afterwards) and re-measured the deliverable from disk: **17/17 PASS,
exit 0.**

Key results:

```
PASS  git status --porcelain empty
PASS  bolt.go == HEAD blob (sha256)   14c1d29458afbc01b816dc8f9e9c7465a2f1a636a69bc1360ee25cbb5dd892b8
PASS  RED-PROOF markers in bolt.go    0
PASS  bolt.go uses 5s Timeout         1
PASS  HEAD == fork-bot ref            5994ddcf4d1ef787741e08c34c3436313e3615d2
PASS  tracked+committed: (all 7 deliverable files)
PASS  gofmt clean load_test.go
PASS  go vet ./wallet/                0
PASS  in-test runtime.Stack dump      1
PASS  guard is 60s                    1
AD-HOC VERIFY: all checks PASS
```

Behavioral re-run on the committed tree:

```
--- PASS: TestLoadWalletTwiceDoesNotDeadlock (9.12s)
ok  github.com/OpenTollGate/gonuts-tollgate/wallet        26.328s
ok  github.com/OpenTollGate/gonuts-tollgate/wallet/storage 92.064s
```

### Note on `wallet/storage/bolt.go` appearing in "changed paths"

A change-tracker may report `wallet/storage/bolt.go` as modified. It is **not**
a net change: that file was temporarily overwritten with the byte-exact
pre-fix revision to produce the RED proof (§1), then restored with
`git checkout --`. Proof it is restored:

- `git status --porcelain` → empty;
- `git diff HEAD -- wallet/storage/bolt.go` → empty;
- on-disk sha256 `14c1d294…892b8` == `git show HEAD:wallet/storage/bolt.go`
  sha256 (identical);
- `grep -c 'RED-PROOF' wallet/storage/bolt.go` → `0`.

The `git status --porcelain` and `git diff HEAD --stat` checks are in the
ad-hoc script itself, so this is re-verified on every run rather than
asserted from memory.

One caveat on the observed green wall time: it drifts with machine load
(`5.3s … 9.1s` across 7 runs). This is exactly the variance that motivated
raising the guard to 60s (§3) — the guard is not a performance assertion.

