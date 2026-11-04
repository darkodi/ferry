package ledger

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
)

func TestParseUSDC(t *testing.T) {
	tests := []struct {
		in      string
		want    Amount
		wantErr error
	}{
		{"0", 0, nil},
		{"1", 1_000_000, nil},
		{"0.5", 500_000, nil},
		{"100.000001", 100_000_001, nil},
		{"50.20", 50_200_000, nil},
		{"007", 7_000_000, nil},
		{"9223372036854.775807", math.MaxInt64, nil},

		{"", 0, ErrInvalidAmount},
		{"-1", 0, ErrInvalidAmount},
		{"+1", 0, ErrInvalidAmount},
		{" 1", 0, ErrInvalidAmount},
		{"1 ", 0, ErrInvalidAmount},
		{".5", 0, ErrInvalidAmount},
		{"5.", 0, ErrInvalidAmount},
		{"1.2.3", 0, ErrInvalidAmount},
		{"1e6", 0, ErrInvalidAmount},
		{"0x10", 0, ErrInvalidAmount},
		{"1,5", 0, ErrInvalidAmount},
		{"0.0000001", 0, ErrInvalidAmount},
		{"NaN", 0, ErrInvalidAmount},
		{"１", 0, ErrInvalidAmount}, // full-width digit
		{strings.Repeat("1", 33), 0, ErrInvalidAmount},

		{"9223372036854.775808", 0, ErrOverflow},
		{"9223372036855", 0, ErrOverflow},
		{"99999999999999999999", 0, ErrOverflow},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseUSDC(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseUSDC(%q) error = %v, want %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParseUSDC(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestAmountString(t *testing.T) {
	tests := []struct {
		in   Amount
		want string
	}{
		{0, "0.000000"},
		{1, "0.000001"},
		{50_200_000, "50.200000"},
		{-49_800_000, "-49.800000"},
		{math.MaxInt64, "9223372036854.775807"},
		{math.MinInt64, "-9223372036854.775808"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Amount(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

func TestAmountAddSubOverflow(t *testing.T) {
	tests := []struct {
		name    string
		op      func() (Amount, error)
		want    Amount
		wantErr bool
	}{
		{"add", func() (Amount, error) { return Amount(2).Add(3) }, 5, false},
		{"add negative", func() (Amount, error) { return Amount(2).Add(-3) }, -1, false},
		{"add overflow", func() (Amount, error) { return Amount(math.MaxInt64).Add(1) }, 0, true},
		{"add underflow", func() (Amount, error) { return Amount(math.MinInt64).Add(-1) }, 0, true},
		{"sub", func() (Amount, error) { return Amount(2).Sub(3) }, -1, false},
		{"sub overflow", func() (Amount, error) { return Amount(math.MaxInt64).Sub(-1) }, 0, true},
		{"sub underflow", func() (Amount, error) { return Amount(math.MinInt64).Sub(1) }, 0, true},
		{"sub min from zero", func() (Amount, error) { return Amount(0).Sub(math.MinInt64) }, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.op()
			if tc.wantErr {
				if !errors.Is(err, ErrOverflow) {
					t.Fatalf("want ErrOverflow, got %d, %v", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// FuzzParseUSDC checks that the parser never panics, and that every
// accepted string equals its exact value computed with big.Rat and
// survives a String/Parse round trip.
func FuzzParseUSDC(f *testing.F) {
	for _, s := range []string{"0", "1", "0.5", "100.000001", "9223372036854.775807", "1e6", "-1", ".5", "", "1.0000000"} {
		f.Add(s)
	}
	scale := new(big.Rat).SetInt64(usdcScale)
	f.Fuzz(func(t *testing.T, s string) {
		a, err := ParseUSDC(s)
		if err != nil {
			return
		}
		if a < 0 {
			t.Fatalf("ParseUSDC(%q) = %d, negative", s, a)
		}
		exact, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("ParseUSDC accepted %q, which is not a number", s)
		}
		exact.Mul(exact, scale)
		if !exact.IsInt() || exact.Num().Cmp(big.NewInt(int64(a))) != 0 {
			t.Fatalf("ParseUSDC(%q) = %d, exact value is %s", s, a, exact.RatString())
		}
		back, err := ParseUSDC(a.String())
		if err != nil || back != a {
			t.Fatalf("round trip of %q: %q -> %d, %v", s, a.String(), back, err)
		}
	})
}
