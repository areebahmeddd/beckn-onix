# Decrypter

`decrypter` implements `definition.Decrypter`. It backs the handler's optional `decrypt` step, which restores the plaintext of an inbound payload that a counterparty's [`encrypter`](../encrypter/README.md) sent as an `encrypted` envelope.

## Config

None. Add the plugin and the `decrypt` step to a receiver module (it also needs `keyManager`), after `validateSign` and before every step that reads the payload:

```yaml
plugins:
  decrypter:
    id: decrypter
steps:
  - validateSign
  - decrypt
  - validateSchema
  - addRoute
  - signAck
```

Bodies without the envelope pass through unchanged. The step rejects an envelope that is not addressed to this subscriber and key, or whose sender is not the request signer. See [CONFIG.md](../../../../CONFIG.md#17-encrypter--decrypter-plugins).

## Security caveats

`Decrypt(ctx, encryptedData, privateKeyBase64, publicKeyBase64)` reverses `encrypter` (X25519 ECDH, raw AES-256 key, AES-ECB, PKCS#7). The ciphertext is not authenticated, so keep `validateSign` before `decrypt`. See the [encrypter caveats](../encrypter/README.md#security-caveats).
