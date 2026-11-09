package solana

import (
	"cmp"
	"math"
	"slices"
	"strconv"

	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/darkodi/ferry/internal/ledger"
)

// Credit is the increase of one watched token account's USDC balance in
// one transaction.
type Credit struct {
	Index   uint16 // position of the account in the transaction's account keys
	Account sol.PublicKey
	Amount  ledger.Amount
}

// Credits compares a transaction's token balances before and after it ran
// and returns the USDC increase of every watched account in it.
//
// Reading balances instead of decoding instructions catches every way
// tokens can arrive (Transfer, TransferChecked, a program's CPI) with one
// rule. The RPC node is not trusted: an entry counts only if its account
// index is in range, its mint is mint, its token owner is owner (Ferry's
// deposit owner), and its program, when given, is the Token program. Amounts
// must be canonical decimal integers; anything else is skipped, never
// rounded. A balance that went down or stayed the same credits nothing.
func Credits(keys []sol.PublicKey, pre, post []rpc.TokenBalance, mint, owner sol.PublicKey, watched func(sol.PublicKey) bool) []Credit {
	before, badBefore := byIndex(pre, len(keys))
	after, badAfter := byIndex(post, len(keys))
	var out []Credit
	for idx, b := range after {
		if badAfter[idx] || badBefore[idx] || !ours(b, mint, owner) || !watched(keys[idx]) {
			continue
		}
		to, ok := ParseAmount(b.UiTokenAmount)
		if !ok {
			continue
		}
		var from uint64
		if a, seen := before[idx]; seen {
			// The account existed before the transaction: same checks.
			if !ours(a, mint, owner) {
				continue
			}
			if from, ok = ParseAmount(a.UiTokenAmount); !ok {
				continue
			}
		}
		if to <= from || to-from > math.MaxInt64 {
			continue
		}
		out = append(out, Credit{Index: idx, Account: keys[idx], Amount: ledger.Amount(to - from)}) //nolint:gosec // checked above
	}
	slices.SortFunc(out, func(a, b Credit) int { return cmp.Compare(a.Index, b.Index) })
	return out
}

// byIndex maps balances by account index, dropping indexes outside keys.
// An index listed twice is returned in bad so the caller skips it.
func byIndex(bs []rpc.TokenBalance, n int) (m map[uint16]rpc.TokenBalance, bad map[uint16]bool) {
	m = make(map[uint16]rpc.TokenBalance, len(bs))
	bad = make(map[uint16]bool)
	for _, b := range bs {
		if int(b.AccountIndex) >= n {
			continue
		}
		if _, dup := m[b.AccountIndex]; dup {
			bad[b.AccountIndex] = true
			continue
		}
		m[b.AccountIndex] = b
	}
	return m, bad
}

func ours(b rpc.TokenBalance, mint, owner sol.PublicKey) bool {
	if b.Mint != mint || b.Owner == nil || *b.Owner != owner {
		return false
	}
	return b.ProgramId == nil || *b.ProgramId == sol.TokenProgramID
}

// ParseAmount reads a raw token amount (the "amount" field, minor units,
// ignoring decimals). It accepts only canonical decimal integers: digits,
// no sign, no leading zeros, at most a u64.
func ParseAmount(a *rpc.UiTokenAmount) (uint64, bool) {
	if a == nil {
		return 0, false
	}
	return parseU64(a.Amount)
}

func parseU64(s string) (uint64, bool) {
	if s == "" || len(s) > 20 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}
