package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"sync/atomic"
)

const (
	DCP_SECRETS_KEY          = "DCP_SECRETS_KEY"
	DCP_SECRETS_TAG_SIZE_KEY = "DCP_SECRETS_TAG_SIZE"
	dcpSecretsNonceSize      = 12
)

var (
	dcpSecretsTagSize = 14
	dcpSecretsGcm     cipher.AEAD
	nonceCounter      = atomic.Int64{}
)

func DecryptSecret(value string) string {
	return decryptSecret(dcpSecretsGcm, value)
}

func nonceFunc(counter *big.Int) []byte {
	nonce := make([]byte, dcpSecretsNonceSize)
	copy(nonce[4:], counter.Bytes())
	return nonce
}

func decryptSecret(gcm cipher.AEAD, value string) string {
	if gcm == nil {
		return value
	}

	decodedBytes, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return value
	}

	counterBytes, cipherText := decodedBytes[:8], decodedBytes[8:]
	counter := big.NewInt(0).SetBytes(counterBytes)

	plainText, openErr := gcm.Open(nil, nonceFunc(counter), cipherText, nil)
	if openErr != nil {
		return openErr.Error()
	}

	return string(plainText)
}

func GetGcmCipher(key []byte) cipher.AEAD {
	if len(key) == 0 {
		return nil
	}

	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil
	}

	gcm, gcmErr := cipher.NewGCMWithTagSize(block, dcpSecretsTagSize)
	if gcmErr != nil {
		return nil
	}

	return gcm
}

// Helper method for tests to allow overriding the secrets key without using an environment variable
func SetSecretsKey(key []byte) {
	keyLen := len(key)
	if keyLen != 16 && keyLen != 24 && keyLen != 32 {
		panic(fmt.Errorf("invalid %s length (%d); must be 16, 24, or 32 bytes long", DCP_SECRETS_KEY, keyLen))
	}

	dcpSecretsGcm = GetGcmCipher(key)
}

func EncryptSecret(value string) string {
	return encryptSecret(dcpSecretsGcm, value)
}

func encryptSecret(gcm cipher.AEAD, value string) string {
	if gcm == nil {
		return value
	}

	secretCount := big.NewInt(nonceCounter.Add(1))
	secretCountBytes := make([]byte, 8)
	secretCount.FillBytes(secretCountBytes)
	nonce := nonceFunc(secretCount)

	encryptedValue := gcm.Seal(secretCountBytes, nonce, []byte(value), nil)
	return base64.StdEncoding.EncodeToString(encryptedValue)
}

func init() {
	if key, found := os.LookupEnv(DCP_SECRETS_KEY); found {
		keyBytes, err := base64.StdEncoding.DecodeString(key)
		if err != nil {
			panic(fmt.Errorf("invalid %s; must be base64 encoded", DCP_SECRETS_KEY))
		}

		SetSecretsKey(keyBytes)
	}

	if sizeString, found := os.LookupEnv(DCP_SECRETS_TAG_SIZE_KEY); found {
		size, err := strconv.ParseInt(sizeString, 10, 32)
		if err != nil {
			panic(fmt.Errorf("invalid %s (%s); must be an integer between 12 and 16 inclusive", DCP_SECRETS_TAG_SIZE_KEY, sizeString))
		}

		if size < 12 || size > 16 {
			panic(fmt.Errorf("invalid %s (%d); must be between 12 and 16 inclusive", DCP_SECRETS_TAG_SIZE_KEY, dcpSecretsTagSize))
		}

		dcpSecretsTagSize = int(size)
	}
}
