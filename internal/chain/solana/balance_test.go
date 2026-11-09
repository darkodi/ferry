package solana_test

import (
	"strconv"
	"testing"

	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/darkodi/ferry/internal/chain/solana"
	"github.com/darkodi/ferry/internal/ledger"
)

var (
	usdcMint     = sol.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	otherMint    = sol.MustPublicKeyFromBase58("So11111111111111111111111111111111111111112")
	depositOwner = testKey(1)
	stranger     = testKey(2)
	sender       = testKey(3)
	depositA     = testKey(4)
	depositB     = testKey(5)
)

// testKey is a fixed, recognisable public key for unit tests.
func testKey(b byte) sol.PublicKey {
	var k sol.PublicKey
	for i := range k {
		k[i] = b
	}
	return k
}

// keys: 0 fee payer, 1 sender token account, 2 and 3 Ferry deposit
// accounts, 4 the token program.
var keys = []sol.PublicKey{stranger, sender, depositA, depositB, sol.TokenProgramID}

func watched(k sol.PublicKey) bool { return k == depositA || k == depositB }

func bal(idx uint16, mint, owner sol.PublicKey, amount string) rpc.TokenBalance {
	o, p := owner, sol.TokenProgramID
	return rpc.TokenBalance{AccountIndex: idx, Mint: mint, Owner: &o, ProgramId: &p,
		UiTokenAmount: &rpc.UiTokenAmount{Amount: amount, Decimals: 6}}
}

func TestCredits(t *testing.T) {
	token2022 := sol.MustPublicKeyFromBase58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")
	foreignProgram := bal(2, usdcMint, depositOwner, "500")
	foreignProgram.ProgramId = &token2022
	noOwner := bal(2, usdcMint, depositOwner, "500")
	noOwner.Owner = nil

	tests := []struct {
		name      string
		pre, post []rpc.TokenBalance
		want      map[sol.PublicKey]ledger.Amount
	}{
		{
			name: "transfer in",
			pre:  []rpc.TokenBalance{bal(1, usdcMint, stranger, "1000"), bal(2, usdcMint, depositOwner, "0")},
			post: []rpc.TokenBalance{bal(1, usdcMint, stranger, "800"), bal(2, usdcMint, depositOwner, "200")},
			want: map[sol.PublicKey]ledger.Amount{depositA: 200},
		},
		{
			name: "account created in the same transaction (no pre balance)",
			post: []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "75")},
			want: map[sol.PublicKey]ledger.Amount{depositA: 75},
		},
		{
			name: "two deposit accounts in one transaction",
			pre:  []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "10"), bal(3, usdcMint, depositOwner, "0")},
			post: []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "15"), bal(3, usdcMint, depositOwner, "7")},
			want: map[sol.PublicKey]ledger.Amount{depositA: 5, depositB: 7},
		},
		{
			name: "balance went down (outflow) credits nothing",
			pre:  []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "500")},
			post: []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "100")},
		},
		{
			name: "unchanged balance credits nothing",
			pre:  []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "500")},
			post: []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "500")},
		},
		{
			name: "another mint is ignored",
			pre:  []rpc.TokenBalance{bal(2, otherMint, depositOwner, "0")},
			post: []rpc.TokenBalance{bal(2, otherMint, depositOwner, "999")},
		},
		{
			name: "mint changed between pre and post is ignored",
			pre:  []rpc.TokenBalance{bal(2, otherMint, depositOwner, "0")},
			post: []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "999")},
		},
		{
			name: "account not owned by Ferry's depositOwner is ignored",
			post: []rpc.TokenBalance{bal(2, usdcMint, stranger, "999")},
		},
		{
			name: "missing owner is ignored",
			post: []rpc.TokenBalance{noOwner},
		},
		{
			name: "another token program is ignored",
			post: []rpc.TokenBalance{foreignProgram},
		},
		{
			name: "unwatched account is ignored",
			post: []rpc.TokenBalance{bal(1, usdcMint, depositOwner, "999")},
		},
		{
			name: "account index out of range is ignored",
			post: []rpc.TokenBalance{bal(9, usdcMint, depositOwner, "999")},
		},
		{
			name: "index listed twice is ignored",
			post: []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "1"), bal(2, usdcMint, depositOwner, "999")},
		},
		{
			name: "non-canonical amounts are ignored",
			post: []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "0100"), bal(3, usdcMint, depositOwner, "1e3")},
		},
		{
			name: "increase above int64 is refused, never truncated",
			post: []rpc.TokenBalance{bal(2, usdcMint, depositOwner, "18446744073709551615")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := solana.Credits(keys, tt.pre, tt.post, usdcMint, depositOwner, watched)
			if len(got) != len(tt.want) {
				t.Fatalf("credits = %+v, want %v", got, tt.want)
			}
			for _, c := range got {
				if w, ok := tt.want[c.Account]; !ok || c.Amount != w || keys[c.Index] != c.Account {
					t.Errorf("credit %+v, want %v", c, tt.want)
				}
			}
		})
	}
}

func TestParseAmount(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{"0", true}, {"1", true}, {"200000000", true}, {"18446744073709551615", true},
		{"", false}, {"00", false}, {"01", false}, {"-1", false}, {"+1", false}, {" 1", false},
		{"1.5", false}, {"1_000", false}, {"0x10", false},
		{"18446744073709551616", false}, {"999999999999999999999", false},
	}
	for _, tt := range tests {
		if _, ok := solana.ParseAmount(&rpc.UiTokenAmount{Amount: tt.in}); ok != tt.ok {
			t.Errorf("ParseAmount(%q) ok = %v, want %v", tt.in, ok, tt.ok)
		}
	}
	if _, ok := solana.ParseAmount(nil); ok {
		t.Error("ParseAmount(nil) accepted")
	}
}

// Amounts come from an untrusted RPC node. Anything accepted must be the
// canonical decimal form of the value it parsed to.
func FuzzParseAmount(f *testing.F) {
	for _, s := range []string{"0", "1", "007", "-1", "18446744073709551615", "18446744073709551616", "1e6", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, ok := solana.ParseAmount(&rpc.UiTokenAmount{Amount: s})
		if ok && strconv.FormatUint(v, 10) != s {
			t.Fatalf("accepted %q as %d", s, v)
		}
	})
}

// Whatever the node reports, a credit is positive, fits int64, equals the
// reported increase, and goes to a watched account at its own index.
func FuzzCredits(f *testing.F) {
	f.Add(uint16(2), "0", "200", uint16(3), "5", "1")
	f.Add(uint16(2), "100", "50", uint16(2), "0", "1")
	f.Add(uint16(9), "", "18446744073709551615", uint16(0), "x", "1")
	f.Fuzz(func(t *testing.T, i1 uint16, pre1, post1 string, i2 uint16, pre2, post2 string) {
		pre := []rpc.TokenBalance{bal(i1, usdcMint, depositOwner, pre1), bal(i2, usdcMint, depositOwner, pre2)}
		post := []rpc.TokenBalance{bal(i1, usdcMint, depositOwner, post1), bal(i2, otherMint, depositOwner, post2)}
		for _, c := range solana.Credits(keys, pre, post, usdcMint, depositOwner, watched) {
			// Amount is int64, so a positive one is within the ledger's range.
			if c.Amount <= 0 || !watched(c.Account) || keys[c.Index] != c.Account {
				t.Fatalf("bad credit %+v", c)
			}
			// Only index i1 can be credited (i2's post is another mint), and
			// only when the indexes differ (a duplicate is refused) and both
			// of i1's amounts parse.
			from, okFrom := solana.ParseAmount(&rpc.UiTokenAmount{Amount: pre1})
			to, okTo := solana.ParseAmount(&rpc.UiTokenAmount{Amount: post1})
			if i1 == i2 || !okFrom || !okTo || c.Index != i1 || uint64(c.Amount) != to-from {
				t.Fatalf("credit %+v from pre %q post %q at %d", c, pre1, post1, i1)
			}
		}
	})
}
