// Package contracts embeds the compiled test contracts used by local
// chains and tests. Regenerate with `make contracts`.
package contracts

import _ "embed"

// TestUSDCBin is the creation bytecode of TestUSDC (hex, no 0x prefix).
//
//go:embed build/TestUSDC.bin
var TestUSDCBin string

// TestUSDCABI is TestUSDC's ABI as JSON.
//
//go:embed build/TestUSDC.abi
var TestUSDCABI string
