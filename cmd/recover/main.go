package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Origami74/gonuts-tollgate/cashu"
	"github.com/Origami74/gonuts-tollgate/cashu/nuts/nut07"
	"github.com/Origami74/gonuts-tollgate/crypto"
	"github.com/Origami74/gonuts-tollgate/wallet"
	"github.com/Origami74/gonuts-tollgate/wallet/client"
	"github.com/joho/godotenv"
)

// --- config / flags ---

func defaultWalletPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".gonuts", "wallet")
	}
	return filepath.Join(home, ".gonuts", "wallet")
}

func loadEnv() {
	wd, err := os.Getwd()
	if err != nil {
		return
	}
	// try CWD .env first, then default wallet path
	for _, p := range []string{
		filepath.Join(wd, ".env"),
		filepath.Join(defaultWalletPath(), ".env"),
	} {
		if _, err := os.Stat(p); err == nil {
			_ = godotenv.Load(p)
			return
		}
	}
}

// --- types ---

type lineResult struct {
	lineNum   int
	tokenStr  string
	mint      string
	amount    uint64
	status    string // "recovered", "spent", "failed", "pending"
	errMsg    string
}

// --- checkstate ---

// computeYs returns hex-encoded HashToCurve(Y) values for each proof's secret,
// which the mint needs to look up in the NUT-07 checkstate endpoint.
func computeYs(proofs cashu.Proofs) ([]string, error) {
	Ys := make([]string, 0, len(proofs))
	for _, proof := range proofs {
		Y, err := crypto.HashToCurve([]byte(proof.Secret))
		if err != nil {
			return nil, fmt.Errorf("HashToCurve for proof (amount %d): %v", proof.Amount, err)
		}
		Ys = append(Ys, hex.EncodeToString(Y.SerializeCompressed()))
	}
	return Ys, nil
}

// checkProofsSpent returns true if ALL proofs in the token are spent at the mint.
// A token is considered "recoverable" only if at least one proof is unspent.
// We treat pending as unspent-with-caution (return false so Receive can try).
func checkProofsSpent(mintURL string, proofs cashu.Proofs) (allSpent bool, err error) {
	Ys, err := computeYs(proofs)
	if err != nil {
		return false, err
	}

	req := nut07.PostCheckStateRequest{Ys: Ys}
	resp, err := client.PostCheckProofState(mintURL, req)
	if err != nil {
		return false, fmt.Errorf("PostCheckProofState: %v", err)
	}

	if len(resp.States) == 0 {
		return false, fmt.Errorf("mint returned no states for %d Ys", len(Ys))
	}

	spendCount := 0
	for _, st := range resp.States {
		if st.State == nut07.Spent {
			spendCount++
		}
	}
	return spendCount == len(resp.States), nil
}

// --- log parsing ---

// parseLogLine splits on " | " (pipe-space) delimiters.
// Expected format: timestamp | mint_url | cashuB_token | error_message
func parseLogLine(line string) (mintURL, tokenStr, errMsg string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", "", false
	}
	parts := strings.Split(line, " | ")
	if len(parts) < 3 {
		return "", "", "", false
	}
	mintURL = strings.TrimSpace(parts[1])
	tokenStr = strings.TrimSpace(parts[2])
	if len(parts) > 3 {
		errMsg = strings.TrimSpace(strings.Join(parts[3:], " | "))
	}
	if tokenStr == "" {
		return "", "", "", false
	}
	return mintURL, tokenStr, errMsg, true
}

// --- main ---

func main() {
	logFile := flag.String("log", "", "Path to log file with rejected tokens (required)")
	walletPath := flag.String("wallet", "", "Wallet directory (default: ~/.gonuts/wallet)")
	mintURL := flag.String("mint", "", "Default/trusted mint URL to swap recovered tokens to (env: MINT_URL)")
	delay := flag.Duration("delay", 500*time.Millisecond, "Delay between mint API calls to avoid 429s")
	dryRun := flag.Bool("dry-run", false, "Check state only; do not call Receive (no wallet needed)")
	noSwap := flag.Bool("no-swap", false, "Receive without swapping to trusted mint (keep proofs from original mint)")
	flag.Parse()

	if *logFile == "" {
		fmt.Fprintln(os.Stderr, "Usage: recover -log <logfile> [-wallet <path>] [-mint <url>] [-delay 500ms] [-dry-run] [-no-swap]")
		flag.Usage()
		os.Exit(1)
	}

	loadEnv()

	if *walletPath == "" {
		if v := os.Getenv("WALLET_PATH"); v != "" {
			*walletPath = v
		} else {
			*walletPath = defaultWalletPath()
		}
	}
	if *mintURL == "" {
		*mintURL = os.Getenv("MINT_URL")
	}

	f, err := os.Open(*logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	// Dry-run mode: no wallet needed, just checkstate
	if !*dryRun {
		if *mintURL == "" {
			fmt.Fprintln(os.Stderr, "Error: -mint or MINT_URL env required (trusted mint to swap to)")
			os.Exit(1)
		}
	}

	var w *wallet.Wallet
	if !*dryRun {
		fmt.Printf("Loading wallet from: %s (trusted mint: %s)\n", *walletPath, *mintURL)
		w, err = wallet.LoadWallet(wallet.Config{
			WalletPath:     *walletPath,
			CurrentMintURL: *mintURL,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading wallet: %v\n", err)
			os.Exit(1)
		}
	}

	var (
		recovered  int
		spent      int
		failed     int
		totalSats  uint64
	)

	scanner := bufio.NewScanner(f)
	// Allow long lines (cashuB tokens can be large)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		mintFromLog, tokenStr, logErrMsg, ok := parseLogLine(scanner.Text())
		if !ok {
			continue
		}

		// Decode the token — cashu.DecodeToken handles both V3 (cashuA) and V4 (cashuB)
		token, err := cashu.DecodeToken(tokenStr)
		if err != nil {
			failed++
			fmt.Printf("[line %d] FAILED  decode error: %v (log msg: %s)\n", lineNum, err, logErrMsg)
			continue
		}

		// Prefer mint URL from the token itself; fall back to log line
		mintURL := token.Mint()
		if mintURL == "" {
			mintURL = mintFromLog
		}
		if mintURL == "" {
			failed++
			fmt.Printf("[line %d] FAILED  no mint URL in token or log line\n", lineNum)
			continue
		}

		amount := token.Amount()
		proofs := token.Proofs()

		fmt.Printf("[line %d] token: %d sats, mint: %s, %d proofs", lineNum, amount, mintURL, len(proofs))

		// Rate limit: sleep before mint call
		time.Sleep(*delay)

		// NUT-07: check if proofs are spent at the mint
		allSpent, err := checkProofsSpent(mintURL, proofs)
		if err != nil {
			failed++
			fmt.Printf(" → FAILED  checkstate: %v\n", err)
			continue
		}

		if allSpent {
			spent++
			fmt.Printf(" → ALREADY SPENT (skipped)\n")
			continue
		}

		if *dryRun {
			recovered++
			totalSats += amount
			fmt.Printf(" → UNSPENT (dry-run, would recover %d sats)\n", amount)
			continue
		}

		// Recover: Receive the token, optionally swapping to trusted mint
		swapToTrusted := !*noSwap
		received, err := w.Receive(token, swapToTrusted)
		if err != nil {
			failed++
			fmt.Printf(" → FAILED  receive: %v\n", err)
			continue
		}

		recovered++
		totalSats += received
		fmt.Printf(" → RECOVERED %d sats\n", received)

		// Rate limit after receive (receive does multiple mint calls internally)
		time.Sleep(*delay)
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading log file: %v\n", err)
	}

	// --- Summary ---
	fmt.Println("\n" + strings.Repeat("=", 50))
	fmt.Println("RECOVERY SUMMARY")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("  Recovered:       %d tokens (%d sats)\n", recovered, totalSats)
	fmt.Printf("  Already spent:    %d tokens\n", spent)
	fmt.Printf("  Failed:           %d tokens\n", failed)
	fmt.Printf("  Total processed:  %d lines\n", lineNum)
	if *dryRun {
		fmt.Println("  (dry-run mode: no tokens were actually received)")
	}
	fmt.Println(strings.Repeat("=", 50))
}