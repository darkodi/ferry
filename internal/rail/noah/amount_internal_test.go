package noah

import (
	"testing"

	"github.com/darkodi/ferry/internal/ledger"
)

func TestAmounts(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want ledger.Amount
		ok   bool
	}{
		{"1010.03", 1_010_030_000, true},
		{"5", 5_000_000, true},
		{"0.000001", 1, true},
		{"1010.030000", 1_010_030_000, true},
		{"1.0000001", 0, false}, // more than 6 decimals: refused, never rounded
		{".5", 0, false},
		{"5.", 0, false},
		{"+5", 0, false},
		{"-5", 0, false},
		{"1e3", 0, false},
		{"", 0, false},
		{"99999999999999999999", 0, false},
	} {
		got, err := parseAmount(tc.in)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("parseAmount(%q) = %d, %v; want %d ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
	for a, want := range map[ledger.Amount]string{
		1_010_030_000: "1010.03", 5_000_000: "5", 1: "0.000001", 0: "0", 120_000: "0.12",
	} {
		if got := formatAmount(a); got != want {
			t.Errorf("formatAmount(%d) = %q, want %q", a, got, want)
		}
	}
}

func TestFiat(t *testing.T) {
	for _, tc := range []struct {
		minor int64
		cur   string
		want  string
		ok    bool
	}{
		{10000, "USD", "100.00", true},
		{5, "EUR", "0.05", true},
		{123456, "GBP", "1234.56", true},
		{100, "JPY", "", false}, // exponent 0: not supported, not guessed
		{0, "USD", "", false},
		{-1, "USD", "", false},
	} {
		got, err := formatFiat(tc.minor, tc.cur)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("formatFiat(%d, %s) = %q, %v", tc.minor, tc.cur, got, err)
		}
	}
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1.2", 120, true}, {"15000", 1_500_000, true}, {"0", 0, true}, {"1.234", 0, false}, {"", 0, false}, {"-1", 0, false}, {"x", 0, false},
	} {
		got, ok := parseFiat(tc.in, "USD")
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseFiat(%q) = %d %v", tc.in, got, ok)
		}
	}
}

// Every amount Noah sends goes through parseAmount: whatever it accepts
// must be a non-negative exact value that formats back to an equal
// number.
func FuzzParseNoahAmount(f *testing.F) {
	for _, s := range []string{"1010.03", "0", ".5", "+1", "1.0000001", "99999999999999.999999", "1e6", "١٢"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := parseAmount(s)
		if err != nil {
			return
		}
		if a < 0 {
			t.Fatalf("parseAmount(%q) = %d, negative", s, a)
		}
		back, err := parseAmount(formatAmount(a))
		if err != nil || back != a {
			t.Fatalf("%q -> %d -> %q -> %d, %v", s, a, formatAmount(a), back, err)
		}
	})
}
