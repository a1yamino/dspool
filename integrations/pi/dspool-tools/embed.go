// Package dspooltools embeds the pi search extension so `dspool pi` can
// provision it for users who install the binary without a source checkout.
package dspooltools

import _ "embed"

// Source is the extension loaded by pi from its extensions directory.
//
//go:embed index.ts
var Source []byte
