package main

import (
	"fmt"

	"minivectordb/internal/xfmt"
)

// Every message formatted during the server tests must equal fmt.Sprintf.
func init() {
	xfmt.Check = func(format string, a []any, got string) {
		if want := fmt.Sprintf(format, a...); got != want {
			panic(fmt.Sprintf("xfmt.Sprintf(%q) = %q, fmt gives %q", format, got, want))
		}
	}
}
