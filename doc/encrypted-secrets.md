DCP supports an optional encrypted secrets mode that can be enabled by passing a Base64 encoded AES key to DCP via the `DCP_SECRETS_KEY` environment variable. When enabled, DCP expects specific secret values to be encrypted and RFC 4648 Base64 encoded using a specific schema.

16, 24, and 32 byte key lengths are supported. If an invalid AES key length is passed to DCP_SECRETS_KEY, DCP will report an error and exit immediately.

Additionally, the AES-GCM tag size can be configured by setting the `DCP_SECRETS_TAG_SIZE` environment variable. The default tag size is 14 bytes, but any value between 12 and 16 bytes is supported. If an invalid tag size is passed to DCP_SECRETS_TAG_SIZE, DCP will report an error and exit immediately.

When DCP is started with `DCP_SECRETS_KEY` set, all secrets will be assumed to be encrypted.

To encrypt a secret for DCP when `DCP_SECRETS_KEY` is enabled, the following steps are required:

## Common setup steps:
1. Generate a new AES key of valid length (16, 24, or 32 bytes)
1. Base64 encode this AES key and pass to DCP via the new `DCP_SECRETS_KEY` environment variable
1. Keep an atomic 64bit integer counter that is used to guarantee a unique nonce value for every encryption operation (atomically incremented on each encryption operation)

## Secret encryption steps:
1. Get a unique counter value by incrementing the global counter from the common steps.
1. Generate a 12 byte nonce value for an encryption operation using the counter value. The first four bytes of the nonce should be zeroed. The final eight bytes should be set to the counter value in big endian format.
   * Example: if the counter value is 1, the nonce would be 0x000000000001
1. AES-GCM encrypt the secret using the AES key and nonce; you'll receive the encrypted bytes and a byte tag used for verification on decryption
1. Concatenate the counter value in big endian format, encrypted bytes, and tag bytes (in that order) into a single byte array
1. Base64 (RFC 4648) encode the new byte array and write it as the secret value

An example of how secret encryption might work in a C# app:

```C#
using System.Buffers.Binary;
using System.Security.Cryptography;
using System.Text;

internal class DcpEncryptedSecretService
{
    private const int TagSize = 14;

    private long _nonceCounter;

    private readonly Lazy<AesGcm> _gcm;

    public DcpEncryptedSecretService()
    {
        AesKey = RandomNumberGenerator.GetBytes(32);
        _gcm = new Lazy<AesGcm>(() =>
        {
            return new AesGcm(AesKey, TagSize);
        });
    }

    public byte[] AesKey { get; private set;}

    public string EncryptSecret(string plainText)
    {
        var gcm = _gcm.Value;

        var counter = Interlocked.Increment(ref _nonceCounter);
        var plainTextBytes = Encoding.UTF8.GetBytes(plainText);

        // Nonce byte size for GCM has a standard of 12 bytes
        var nonce = new byte[AesGcm.NonceByteSizes.MaxSize].AsSpan();
        nonce.Clear();

        // Write the counter to the end of the nonce in big-endian format
        BinaryPrimitives.WriteInt64BigEndian(nonce.Slice(AesGcm.NonceByteSizes.MaxSize - sizeof(long)), counter);

        // Prepare the encrypted data buffer (count + encrypted text + tag)
        var encryptedData = new byte[sizeof(long) + plainTextBytes.Length + TagSize].AsSpan();

        // Copy the counter to the beginning of the buffer
        nonce.Slice(4).CopyTo(encryptedData);

        // Encrypt the plain text
        gcm.Encrypt(nonce, plainTextBytes, encryptedData.Slice(8, plainTextBytes.Length), encryptedData.Slice(8 + plainTextBytes.Length, TagSize));

        // Return the Base64 encoded encrypted data
        return Convert.ToBase64String(encryptedData);
    }
}
```