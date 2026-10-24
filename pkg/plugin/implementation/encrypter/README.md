# Encrypter

`encrypter` implements `definition.Encrypter`. It backs the handler's optional `encrypt` step, which encrypts an outbound payload for a counterparty that publishes an `encr_public_key` in the registry. The receiving side is [`decrypter`](../decrypter/README.md).

## Config

None. Add the plugin and the `encrypt` step to a caller module (it also needs `keyManager` and `registry`), after every step that reads the payload and before `sign`:

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

Recipients without a discoverable key get plaintext; a registry outage fails the request. Behaviour, envelope format, key provisioning and the `dediregistry` key-id requirement are in [CONFIG.md](../../../../CONFIG.md#17-encrypter--decrypter-plugins).

## Security caveats

`Encrypt(ctx, data, privateKeyBase64, publicKeyBase64)` derives an AES-256 key directly from a static-static X25519 ECDH secret and encrypts in AES-ECB with PKCS#7 padding. There is no KDF, IV or authentication tag, so the output:

- is deterministic and leaks repeated 16-byte blocks;
- has no integrity of its own (the Beckn signature over the ciphertext provides it);
- has no forward secrecy.

Moving to an authenticated scheme (HPKE, or ephemeral X25519 + HKDF + AES-GCM) is an open follow-up.
