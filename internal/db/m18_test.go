package db

import "testing"

// New dimension, population and seed; retain independent exact oracle,
// filtered/unfiltered recall floors and <=75% eligible candidate checks.
func TestM18ANNBroaderRSS(t *testing.T) {
	testGaussianANN(t, true, 4096, 96, 32, 1801, true)
}
