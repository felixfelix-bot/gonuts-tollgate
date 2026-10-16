# PROGRESS — GNT-LOADDEADLOCK-1 (reproduce LoadWallet deadlock, regression test + root cause)

Task: reproduce `wallet.LoadWallet()` double-call deadlock (issue c03rad0r/gonuts-tollgate#1)
with a deterministic failing regression test + root-cause evidence.

Worktree: /home/c03rad0r/worktrees/gnt-loaddeadlock
Branch:   worker-base/GNT-LOADDEADLOCK-1 (off local main 934ce46)
Remotes:  origin=OpenTollGate/gonuts-tollgate (pull-only, READ), fork-bot=felixfelix-bot/gonuts-tollgate (push)

## Log

- [1] RECON: local main HEAD 934ce46 ALREADY contains the deliverable —
      `wallet/load_test.go` TestLoadWalletTwiceDoesNotDeadlock + `docs/loadwallet-deadlock-rootcause.md`
      + the fix 60a7405 (bolt.Options{Timeout:5s}). Neither is on origin/main.
      -> files read: wallet/load_test.go, wallet/wallet.go, wallet/storage/bolt.go, docs/loadwallet-deadlock-rootcause.md
- [2] DUPLICATE CHECK: origin/main is 23 commits AHEAD and has its OWN fix (ErrDBLocked sentinel +
      boltOpenTimeout const) and its OWN regression test
      `TestLoadWallet_SecondInProcessCallWithoutShutdownDoesNotBlock` in wallet/wallet_test.go.
      => this dispatch is a duplicate of work that exists in two forms. Per
      governance/duplicate-dispatch-verification: verify, don't blindly redo.
- [3] GREEN VERIFIED: `go test -run TestLoadWalletTwiceDoesNotDeadlock ./wallet/` -> PASS,
      log "second LoadWallet returned error: InitStorage: error setting bolt db: timeout".
- [4] RED VERIFIED (ad-hoc patch): reverted InitBolt to `nil` -> test FAILED with
      "LoadWallet deadlocked on second call: blocked on bbolt file lock (flock) for >10s".
      Goroutine dump (SIGQUIT) captured -> blocked goroutine in
      bbolt.flock bolt_unix.go:44 <- bbolt.Open db.go:248 <- storage.InitBolt bolt.go
      <- InitStorage wallet.go:76 <- LoadWallet wallet.go:85 <- load_test.go:121.
- [5] BANKED: pushed worker-base/GNT-LOADDEADLOCK-1 to fork-bot (first push, deliverable off-box).
- [6] DEFECT FOUND + FIXED: the 10s inner deadlock guard was FLAKY. GREEN runs measured
      5.30s / 5.62s / 6.17s / 6.68s / 8.35s, plus one 22.63s outlier under parallel build load.
      A 10s guard is within ~1.6x of a legitimate green run => would report a FALSE deadlock.
      Raised to named `deadlockGuard = 60 * time.Second` and made the guard self-contained:
      it now calls runtime.Stack in-test instead of relying on an outer -timeout.
      files touched: wallet/load_test.go (guard + import runtime, drop fmt)
- [7] PRISTINE RED PROOF: rebuilt the pre-fix binary from the byte-exact pre-fix file
      (`git show 4b7d125:wallet/storage/bolt.go`, sha256 2efbfa47...8e8e), NOT a hand edit.
      Dump shows `bolt.go:38` (the real pre-fix line) with args `..., 0600, nil` and the
      0x0 timeout threaded into bbolt.Open/flock.
      New in-test guard also RED-verified: `--- FAIL ... (62.21s)` + full dump.
      files touched: wallet/storage/bolt.go (restored byte-exact via git checkout afterwards)
- [8] CORRECTED ROOT-CAUSE NOTE: the original note cited bolt.go:38 for the FIXED tree; it is
      L38 pre-fix and L39 post-fix (the fix adds a `time` import). Both now stated with their
      tree. Also recorded that the primitive is an OS advisory file lock (syscall.Flock LOCK_EX),
      not a Go mutex/Once/channel — hence the `[sleep]` frame rather than `[semacquire]`.
      files touched: docs/loadwallet-deadlock-rootcause.md
- [9] SUITE VERIFIED: `go test ./wallet/ ./wallet/storage/` -> ok 17.1s / ok 19.5s.
      `go build ./wallet/... ./crypto/... ./cashu/...` -> exit 0.
      `go build ./...` (whole repo incl. mint) does NOT complete offline (EXIT=124, wants
      uncached modules like pgpassfile/docker/cli). PRE-EXISTING environment limit, unrelated
      to this change (which touches only tests + docs).
- [10] COMMITTED + PUSHED: dd04e02 (6 files, +688/-31). Remote verified:
      fork-bot refs/heads/worker-base/GNT-LOADDEADLOCK-1 == dd04e028f99f8e089ef4c65b4222899541313131
- [11] RECONCILIATION documented in the root-cause note: upstream already ships both a better fix
      (typed ErrDBLocked sentinel) and its own regression test; 4b7d125 IS an ancestor of
      origin/main, so upstream and this branch share the pre-fix base. Recommendation: do NOT
      PR this against origin/main (would re-apply the weaker fix + duplicate test). Manager call.

## Status: deliverable complete (both acceptance criteria met), verified, committed, pushed.
## Open for the manager: whether to rebase the hardened test onto origin/main (see note §Reconciliation).
