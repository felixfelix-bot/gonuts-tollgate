# PROGRESS — GNT-LOADDEADLOCK-1 (reproduce LoadWallet deadlock, regression test + root cause)

Task: reproduce `wallet.LoadWallet()` double-call deadlock (issue c03rad0r/gonuts-tollgate#1)
with a deterministic failing- regression test + root-cause evidence.

Worktree: /home/c03rad0r/worktrees/gnt-loaddeadlock
Branch:   worker-base/GNT-LOADDEADLOCK-1 (off local main 934ce46)
Remotes:  origin=OpenTollGate/gonuts-tollgate (pull-only), fork-bot=felixfelix-bot/gonuts-tollgate (push)

## Log

- [1] RECON: local main HEAD 934ce46 ALREADY contains the deliverable —
      `wallet/load_test.go` TestLoadWalletTwiceDoesNotDeadlock + `docs/loadwallet-deadlock-rootcause.md`
      + the fix 60a7405 (bolt.Options{Timeout:5s}). Neither is on origin/main.
      → files touched: (read only) wallet/load_test.go, wallet/wallet.go, wallet/storage/bolt.go, docs/loadwallet-deadlock-rootcause.md
- [2] DUPLICATE CHECK: origin/main is 23 commits AHEAD and has its OWN fix (ErrDBLocked sentinel +
      boltOpenTimeout const) and its OWN regression test
      `TestLoadWallet_SecondInProcessCallWithoutShutdownDoesNotBlock` in wallet/wallet_test.go.
      ⇒ this dispatch is a duplicate of work that exists in two forms. Per
      governance/duplicate-dispatch-verification: verify, don't blindly redo.
- [3] GREEN VERIFIED: `go test -run TestLoadWalletTwiceDoesNotDeadlock ./wallet/` → PASS (5.52s),
      log "second LoadWallet returned error: InitStorage: error setting bolt db: timeout".
      files touched: none (verification only)
- [4] RED VERIFIED (ad-hoc patch): reverted InitBolt to `nil` options → test FAILED with
      "LoadWallet deadlocked on second call: blocked on bbolt file lock (flock) for >10s".
      Goroutine dump (SIGQUIT) captured → blocked goroutine in
      bbolt.flock bolt_unix.go:44 ← bbolt.Open db.go:248 ← storage.InitBolt bolt.go ← InitStorage wallet.go:76
      ← LoadWallet wallet.go:85 ← load_test.go:121 (test goroutine).
      files touched: wallet/storage/bolt.go (reverted immediately, restored via git checkout)
- [5] BANKED: pushed worker-base/GNT-LOADDEADLOCK-1 to fork-bot (new branch). Deliverable now off-box.
