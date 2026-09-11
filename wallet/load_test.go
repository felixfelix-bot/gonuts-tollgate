//go:build !integration

package wallet

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenTollGate/gonuts-tollgate/cashu/nuts/nut01"
	"github.com/OpenTollGate/gonuts-tollgate/cashu/nuts/nut02"
)

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
// We run the second call in a goroutine with a select on a 10s
// time.After. If the deadlock is present the test FAILS with a
// timeout instead of hanging. When the bug is fixed (InitBolt uses
// a Timeout) the second call returns a non-nil error quickly,
// and the test PASSES.
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
	case <-time.After(10 * time.Second):
		// DEADLOCK REPRODUCED: the second LoadWallet is stuck inside
		// bolt.Open → flock because the first call still holds the
		// file lock and Timeout was 0 (wait forever).
		//
		// The goroutine dump from `go test -timeout 30s` will show
		// the blocked goroutine in bolt.Open / syscall.Flock.
		t.Fatal(fmt.Sprintf(
			"LoadWallet deadlocked on second call: blocked on bbolt file lock (flock) for >10s — "+
				"see goroutine dump for bolt.Open in wallet/storage/bolt.go:InitBolt (keyset=%s)", keysetID))
	}
}