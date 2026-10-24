# Decrypter

`decrypter` implements `definition.Decrypter`. It backs the handler's optional `decrypt` step, which restores the plaintext of an inbound Beckn payload that a counterparty's [`encrypter`](../encrypter/README.md) sent as an encrypted envelope.

## Enabling

Decryption is opt-in per module. Add the plugin and the `decrypt` step to a **receiver** module. The step needs the module's `keyManager`. Place it after `validateSign` (start-up fails without it) and before every step that reads the payload:

```yaml
plugins:
  decrypter:
    id: decrypter
steps:
  - validateSign
  - decrypt
  - validateSchema
  - checkPolicy
  - addRoute
  - signAck
```

The step only acts on bodies that carry the top-level `encrypted` envelope. Encryption is opportunistic: senders encrypt only for recipients whose key they can discover. Unencrypted messages therefore pass through unchanged, so senders that don't encrypt keep working. The step reads the sender's `encr_public_key` from the registry via `keyManager.LookupNPKeys(senderId, senderKeyId)`. It also checks that:
- the envelope is addressed to this subscriber and key;
- the request signer (`Authorization` `keyId` subscriber and key) is the envelope's sender.

The full behaviour, envelope format and key provisioning are in [CONFIG.md, Encrypter / Decrypter](../../../../CONFIG.md#17-encrypter--decrypter-plugins-optional-message-level-encryption).

## Config

None.

## Algorithm and security caveats

`Decrypt(ctx, encryptedData, privateKeyBase64, publicKeyBase64)` reverses [`encrypter`](../encrypter/README.md): it uses X25519 ECDH to derive a raw AES-256 key, then decrypts in AES-ECB and strips the PKCS#7 padding. The scheme provides no authentication of its own, so keep `validateSign` before `decrypt`. The signature covers the ciphertext and rejects tampered messages before decryption. See the encrypter README for the full list of caveats and the suggested follow-up.
