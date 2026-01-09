/*---------------------------------------------------------------------------------------------
 *  Copyright (c) Microsoft Corporation. All rights reserved.
 *  Licensed under the MIT License. See LICENSE in the project root for license information.
 *--------------------------------------------------------------------------------------------*/

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

// Returns a random int64 between [0, max)
func MakeRandomInt64(max int64) (int64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(max))
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}
