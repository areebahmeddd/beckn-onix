# Encrypter

`encrypter` implements `definition.Encrypter`. It backs the handler's optional `encrypt` step, which encrypts an outbound Beckn payload for the counterparty when the counterparty has published an `encr_public_key` in the registry. Its counterpart on the receiving side is [`decrypter`](../decrypter/README.md).

## Enabling

Encryption is opt-in per module. Add the plugin and the `encrypt` step to a **caller** module. The step needs the module's `keyManager` and `registry`. Place it after every step that reads the payload and before `sign`:

```yaml
plugins:
  encrypter:
    id: encrypter
steps:
  - validateSchema
  - addRoute
  - encrypt
  - sign
```

If you omit the plugin and the step, encryption stays disabled. Even when it is enabled, a message goes out unencrypted in either of these cases:
- there's no single counterparty, as with a broadcast `search`;
- the counterparty's registry record has no `encr_public_key`.

The full behaviour is in [CONFIG.md, Encrypter / Decrypter](../../../../CONFIG.md#17-encrypter--decrypter-plugins-optional-message-level-encryption), including the on-the-wire envelope, step ordering, key provisioning and registry requirements.

## Config

None.

## Algorithm and security caveats

`Encrypt(ctx, data, privateKeyBase64, publicKeyBase64)` works in three stages:
1. It derives a 32-byte shared secret with X25519 ECDH from the sender's encryption private key and the recipient's encryption public key, both base64.
2. It uses that secret directly as an AES-256 key.
3. It encrypts `data` in **ECB mode with PKCS#7 padding** and returns the result as base64.

The scheme has no IV, no KDF and no authentication tag, and the ECDH is static-static, so:
- the output is deterministic;
- it leaks repeated 16-byte blocks;
- it has no integrity protection of its own;
- it has no forward secrecy.

Inside the ONIX pipeline, integrity comes from the Beckn signature, which is computed over the ciphertext (`encrypt` before `sign`, `validateSign` before `decrypt`). Moving to an authenticated scheme such as HPKE (RFC 9180), or ephemeral X25519 + HKDF + AES-GCM, is an open follow-up.
