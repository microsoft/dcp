package randdata

import (
	"crypto/rand"
	"math/big"
)

const lowercaseLetters = "abcdefghijklmnopqrstuvwxyz"

func MakeRandomString(length uint32) ([]byte, error) {
	retval := make([]byte, length)

	for i := 0; i < int(length); i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(lowercaseLetters))))
		if err != nil {
			return nil, err
		}
		retval[i] = lowercaseLetters[n.Int64()]
	}

	return retval, nil
}
