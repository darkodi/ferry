package noah

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/darkodi/ferry/internal/ledger"
)

// errAmount: Noah sent an amount Ferry cannot represent exactly.
var errAmount = errors.New("noah: bad amount")

// parseAmount reads a crypto amount from Noah ("1010.03") into USDC minor
// units. It uses the ledger's strict parser: digits, at most one dot, at
// most 6 decimals. Forms Noah's pattern allows but never sends in
// practice (".5", "5.", "+5") are refused rather than guessed at, and a
// value with more than 6 decimals is refused rather than rounded.
func parseAmount(s string) (ledger.Amount, error) {
	a, err := ledger.ParseUSDC(s)
	if err != nil {
		return 0, fmt.Errorf("%w %q: %w", errAmount, s, err)
	}
	return a, nil
}

// formatAmount writes a USDC amount the way Noah writes it: no trailing
// zeros, no trailing dot ("1010.03", "5"). Sending back the authorized
// amount prepare returned therefore reproduces Noah's own string.
func formatAmount(a ledger.Amount) string {
	s := a.String() // always has 6 decimals
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// fiatExponents lists the fiat currencies the adapter can price, with
// their minor-unit exponent. Ferry stores fiat in minor units (cents);
// Noah takes decimal strings.
var fiatExponents = map[string]int{"USD": 2, "EUR": 2, "GBP": 2}

// formatFiat writes minor units as a decimal string: 10000 USD -> "100.00".
func formatFiat(minor int64, currency string) (string, error) {
	exp, ok := fiatExponents[currency]
	if !ok {
		return "", fmt.Errorf("noah: fiat currency %s not supported", currency)
	}
	if minor <= 0 {
		return "", fmt.Errorf("noah: fiat amount must be positive, got %d", minor)
	}
	scale := int64(1)
	for range exp {
		scale *= 10
	}
	return fmt.Sprintf("%d.%0*d", minor/scale, exp, minor%scale), nil
}

// parseFiat reads a fiat decimal (a channel limit such as "1.2" or
// "15000") into minor units. It reports false for anything it cannot
// read exactly; callers then leave the check to Noah.
func parseFiat(s, currency string) (int64, bool) {
	exp, ok := fiatExponents[currency]
	if !ok || s == "" || len(s) > 24 {
		return 0, false
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > exp {
		return 0, false
	}
	frac += strings.Repeat("0", exp-len(frac))
	v, err := strconv.ParseUint(whole+frac, 10, 63)
	if err != nil || whole == "" {
		return 0, false
	}
	return int64(v), true
}
