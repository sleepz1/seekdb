package db

import "testing"

// Keep the original M16 seed, dimensions, rows, queries and both filter groups.
func TestM17ANNGaussian64(t *testing.T) {
	testGaussian64(t, true)
}
