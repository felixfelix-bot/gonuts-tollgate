//go:build !integration

package wallet

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/OpenTollGate/gonuts-tollgate/cashu/nuts/nut01"
	"github.com/OpenTollGate/gonuts-tollgate/cashu/nuts/nut02"
)

// deadlockGuard bounds how long the second LoadWallet is allowed to take.
// A genuine deadlock blocks forever, so any finite guard detects it; the
// value is chosen to survive slow/loaded machines (see the test comment).
// Exported nowhere — test-local.
const deadlockGuard = 60 * time.Second

// mockMintServer returns an httptest.Server that serves keyset/keys
// endpoints with a valid keyset derived from a deterministic seed.
func mockMintServer() (*httptest.Server, string) {
	ks := generateWalletKeyset("regression-test-keyset-seed", "0/0/0", true, "")

	// Build nut01.GetKeysResponse for GET /v1/keys (active keysets with keys).
	keysMap := make(map[uint64]string, len(ks.PublicKeys))
	for amount, pk := range ks.PublicKeys {
		keysMap[amount] = hex.EncodeToString(pk.SerializeCompressed())
	}
	keysResp := nut01.GetKeysResponse{
		Keysets: []nut01.Keyset{{
			Id:          ks.Id,
			Unit:        ks.Unit,
			Keys:        ks.PublicKeys,
			Active:      true,
			InputFeePpk: ks.InputFeePpk,
		}},
	}
	keysJSON, _ := json.Marshal(keysResp)

	// Build nut02.GetKeysetsResponse for GET /v1/keysets (list, no keys).
	keysetsResp := nut02.GetKeysetsResponse{
		Keysets: []nut02.Keyset{{
			Id:          ks.Id,
			Unit:        ks.Unit,
			Active:      true,
			InputFeePpk: ks.InputFeePpk,
		}},
	}
	keysetsJSON, _ := json.Marshal(keysetsResp)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/keys":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(keysJSON)
		case "/v1/keysets":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(keysetsJSON)
		default:
			// Also handle GET /v1/keys/{id} as fallback
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(keysJSON)
		}
	}))

	return server, ks.Id
}

// TestLoadWalletTwiceDoesNotDeadlock reproduces issue
// c03rad0r/gonuts-tollgate#1: calling wallet.LoadWallet() twice on
// the same wallet directory in one process deadlocks because the
// first call holds the bbolt file lock and the second bolt.Open()
// waits forever (default Timeout=0).
//
// The first LoadWallet() succeeds and returns a *Wallet whose DB
// remains open (the caller never calls Close()). The second
// LoadWallet() blocks inside InitStorage → bolt.Open → flock.
//
// We run the second call in a goroutine with a select on a
// deadlockGuard time.After. If the deadlock is present the test FAILS
// with a timeout instead of hanging. When the bug is fixed (InitBolt
// uses a Timeout) the second call returns a non-nil error quickly,
// and the test PASSES.
//
// The guard is deliberately generous (60s) rather than tight. In the
// fixed state the second call returns after the 5s bolt lock timeout,
// so the expected green wall time is ~5-9s — but a loaded machine can
// stretch that well past 10s (a 22.6s run was observed under heavy
// parallel build load), which a tight guard would misreport as a
// deadlock. A generous guard costs nothing for detection: a real
// deadlock blocks FOREVER, so any finite guard still fails it.
//
// Recommended invocation (outer -timeout must exceed the guard so the
// t.Fatal diagnostic fires first; it prints the fix hint and the test
// goroutine state):
//
//	go test -timeout 90s -run TestLoadWalletTwiceDoesNotDeadlock ./wallet/
//
// For the raw goroutine dump of the blocked goroutine instead, run the
// pre-fix code with an outer timeout below the guard, e.g.
// `go test -timeout 30s ...`, or SIGQUIT the test binary directly.
func TestLoadWalletTwiceDoesNotDeadlock(t *testing.T) {
	mint, keysetID := mockMintServer()
	defer mint.Close()
	t.Logf("mock mint at %s, keyset id=%s", mint.URL, keysetID)

	dir := t.TempDir()
	walletPath := filepath.Join(dir, "wallet")
	if err := os.MkdirAll(walletPath, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	cfg := Config{
		WalletPath:     walletPath,
		CurrentMintURL: mint.URL,
	}

	// First LoadWallet succeeds. The returned *Wallet keeps the bbolt
	// file lock open for its lifetime (no Close before second call).
	w1, err := LoadWallet(cfg)
	if err != nil {
		t.Fatalf("first LoadWallet: %v", err)
	}
	// Deliberately do NOT close w1 — this simulates the real scenario
	// from issue #1 where two callers race on the same directory.
	_ = w1

	// Second LoadWallet must either return an error (if bolt timeout
	// is configured) or succeed — but it must NOT block forever.
	type result struct {
		w   *Wallet
		err error
	}
	done := make(chan result, 1)

	go func() {
		w, err := LoadWallet(cfg)
		done <- result{w, err}
	}()

	select {
	case res := <-done:
		// If we get here, the second LoadWallet did not deadlock.
		// It may return an error (timeout) or succeed (if the lock
		// was somehow released). Either way, not blocking is the
		// property we're testing.
		if res.err != nil {
			t.Logf("second LoadWallet returned error (expected when lock is held): %v", res.err)
		} else {
			t.Logf("second LoadWallet succeeded")
			if res.w != nil {
				_ = res.w.Shutdown()
			}
		}
	case <-time.After(deadlockGuard):
		// DEADLOCK REPRODUCED: the second LoadWallet is stuck inside
		// bolt.Open → flock because the first call still holds the
		// file lock and Timeout was 0 (wait forever).
		//
		// Dump every goroutine so the blocked one is visible right in
		// the test output, rather than relying on the caller to pass
		// an outer -timeout that happens to fire first.
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf(
			"LoadWallet deadlocked on second call: blocked on bbolt file lock (flock) for >%s — "+
				"see goroutine dump for bolt.Open in wallet/storage/bolt.go:InitBolt (keyset=%s)\n"+
				"goroutine dump:\n%s", deadlockGuard, keysetID, buf[:n])
	}
}
