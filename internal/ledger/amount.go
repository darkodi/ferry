package ledger

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Amount is money in minor units. For USDC one unit is a millionth, so
// 1.5 USDC is Amount(1_500_000). It is signed because house account
// balances may go negative; posting amounts must be positive.
type Amount int64

// USDCDecimals is the number of decimal places of USDC.
const USDCDecimals = 6

const usdcScale = 1_000_000 // 10^USDCDecimals

// maxAmountLen bounds the input to ParseUSDC. The largest Amount has 13
// integer digits once divided by 10^6, so 32 bytes is generous.
const maxAmountLen = 32

// Add returns a + b, or ErrOverflow if the result does not fit in int64.
func (a Amount) Add(b Amount) (Amount, error) {
	s := a + b
	// Signed overflow wraps around in Go, so detect it after the fact:
	// adding a positive number must not make the result smaller, and
	// adding a negative one must not make it larger.
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	return s, nil
}

// Sub returns a - b, or ErrOverflow if the result does not fit in int64.
func (a Amount) Sub(b Amount) (Amount, error) {
	s := a - b
	if (b > 0 && s > a) || (b < 0 && s < a) {
		return 0, fmt.Errorf("%w: %d - %d", ErrOverflow, a, b)
	}
	return s, nil
}

// ParseUSDC parses a non-negative decimal string such as "12", "0.5" or
// "100.000001" into an exact Amount. It never goes through float64.
//
// Accepted: one or more digits, optionally followed by "." and 1 to 6
// digits. Rejected: signs, exponents, spaces, "." without digits on both
// sides, more than 6 decimals, and values that overflow int64.
func ParseUSDC(s string) (Amount, error) {
	if len(s) == 0 || len(s) > maxAmountLen {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || !allDigits(whole) || (hasDot && (frac == "" || !allDigits(frac))) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	if len(frac) > USDCDecimals {
		return 0, fmt.Errorf("%w: %q has more than %d decimals", ErrInvalidAmount, s, USDCDecimals)
	}

	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > math.MaxInt64/usdcScale {
		return 0, fmt.Errorf("%w: %q", ErrOverflow, s)
	}
	var f int64
	if frac != "" {
		// Right-pad to 6 digits: "5" means 500000 millionths.
		f, err = strconv.ParseInt(frac+strings.Repeat("0", USDCDecimals-len(frac)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
		}
	}
	return Amount(w * usdcScale).Add(Amount(f))
}

// String formats the amount with exactly six decimals, e.g. "1.500000".
func (a Amount) String() string {
	sign := ""
	// Work in uint64 so math.MinInt64 has an absolute value.
	u := uint64(a) //nolint:gosec // deliberate two's-complement conversion, negated below for a < 0
	if a < 0 {
		sign = "-"
		u = -u
	}
	return fmt.Sprintf("%s%d.%06d", sign, u/usdcScale, u%usdcScale)
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
