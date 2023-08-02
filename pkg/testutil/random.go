package testutil

import (
	"crypto/rand"
	"math/big"
	"testing"
)

const lowercaseLetters = "abcdefghijklmnopqrstuvwxyz"

func GetRandLetters(t *testing.T, count int) string {
	retval := make([]byte, count)

	for i := 0; i < count; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(lowercaseLetters))))
		if err != nil {
			t.Fatalf("Count not create random string: %v", err)
		}
		retval[i] = lowercaseLetters[n.Int64()]
	}

	return string(retval)
}
