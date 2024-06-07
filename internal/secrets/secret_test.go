package secrets

import (
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEncryptedSecretDecryption(t *testing.T) {
	buffer := make([]byte, 24)
	read, err := rand.Read(buffer)
	require.NoError(t, err)
	require.Equal(t, read, len(buffer))

	gcm := GetGcmCipher(buffer)
	require.NotNil(t, gcm)

	testValue := "this is a value that should be encrypted and decrypted"

	encryptedValue := encryptSecret(gcm, testValue)
	require.NotEqual(t, testValue, encryptedValue)

	decryptedValue := decryptSecret(gcm, encryptedValue)
	require.Equal(t, testValue, decryptedValue)
}

func TestSecretNoEncryption(t *testing.T) {
	buffer := make([]byte, 0)

	testValue := "this is a value that should not be encrypted"
	secretValue := decryptSecret(GetGcmCipher(buffer), testValue)
	require.Equal(t, secretValue, testValue)
}
