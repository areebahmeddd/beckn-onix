package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// Optional message-level encryption (issue #727).
//
// The "encrypt" step (caller modules) and the "decrypt" step (receiver
// modules) are opt-in: they only exist in a pipeline when they are listed in
// the handler's steps, which in turn requires the encrypter / decrypter
// plugins to be configured. Encryption is further gated per message on the
// counterparty (opportunistic encryption): the encrypt step only encrypts
// when the registry yields an encryption public key (encr_public_key) for the
// recipient, and fails closed only when the registry cannot be reached; the
// decrypt step only acts on payloads that carry the encrypted envelope.
// Everything else passes through unchanged, so encrypting and non-encrypting
// participants interoperate.
//
// # Wire format (ONIX-local, version "onix-v1")
//
// The Beckn "context" object stays in plaintext (routing, signature identity
// checks, payload storage and telemetry all read it). Every other top-level
// member of the body ("message", "error", ...) is serialised as a single JSON
// object, encrypted, and replaced by one "encrypted" member:
//
//	{
//	  "context": { ...unchanged... },
//	  "encrypted": {
//	    "version":        "onix-v1",
//	    "senderId":       "<sender subscriber_id>",
//	    "senderKeyId":    "<sender registry key_id>",
//	    "recipientId":    "<recipient subscriber_id>",
//	    "recipientKeyId": "<recipient registry key_id>",
//	    "ciphertext":     "<base64 output of the encrypter plugin>"
//	  }
//	}
//
// The sender and recipient key ids are hints that let the recipient resolve
// the sender's registered encryption public key (the encrypter/decrypter
// plugins derive the content key with static-static ECDH, so the recipient
// needs it) and detect a recipient-key mismatch up front.
//
// # Ordering
//
// Encryption happens before signing and decryption after signature
// validation ("encrypt-then-sign"): the Authorization signature covers the
// exact bytes on the wire, i.e. the envelope, so the receiver authenticates
// the ciphertext before it is ever decrypted, and the signer identity is
// bound to the envelope's senderId. Steps that need the plaintext payload
// (see plaintextSteps) must run before "encrypt" and after "decrypt".
// validateEncryptionStepOrder enforces both rules at start-up.
const (
	// encryptedEnvelopeKey is the top-level body member that carries the
	// encrypted envelope. Its presence is the marker the decrypt step keys on.
	encryptedEnvelopeKey = "encrypted"
	// encryptedEnvelopeVersion identifies the ONIX-local envelope format.
	encryptedEnvelopeVersion = "onix-v1"
	// becknContextKey is the top-level body member that is never encrypted.
	becknContextKey = "context"
)

// encryptedEnvelope is the value of the "encrypted" body member.
type encryptedEnvelope struct {
	Version        string `json:"version"`
	SenderID       string `json:"senderId"`
	SenderKeyID    string `json:"senderKeyId"`
	RecipientID    string `json:"recipientId"`
	RecipientKeyID string `json:"recipientKeyId,omitempty"`
	Ciphertext     string `json:"ciphertext"`
}

// validate checks that every field the decrypt step relies on is present.
func (e *encryptedEnvelope) validate() error {
	if e.Version != encryptedEnvelopeVersion {
		return fmt.Errorf("unsupported encrypted envelope version %q (expected %q)", e.Version, encryptedEnvelopeVersion)
	}
	switch {
	case e.SenderID == "":
		return errors.New("encrypted envelope is missing senderId")
	case e.SenderKeyID == "":
		return errors.New("encrypted envelope is missing senderKeyId")
	case e.RecipientID == "":
		return errors.New("encrypted envelope is missing recipientId")
	case e.Ciphertext == "":
		return errors.New("encrypted envelope is missing ciphertext")
	}
	return nil
}

// plaintextSteps are the built-in steps that inspect the message payload and
// therefore must see plaintext: they must run before "encrypt" and after
// "decrypt".
var plaintextSteps = []string{"validateSchema", "checkPolicy", "mediateSchema", "transformPayload"}

// validateEncryptionStepOrder rejects step lists in which "encrypt" or
// "decrypt" are placed where they would break signing or plaintext-only
// steps. The ciphertext carries no integrity protection of its own, so
// "encrypt" requires a later "sign" and "decrypt" an earlier "validateSign".
// Step lists without either step are always valid.
func validateEncryptionStepOrder(steps []string) error {
	if i := slices.Index(steps, "encrypt"); i >= 0 {
		if j := slices.Index(steps, "sign"); j < i {
			return fmt.Errorf("invalid config: step \"encrypt\" must be followed by \"sign\" so the signature covers the encrypted payload")
		}
		for _, p := range plaintextSteps {
			if j := slices.Index(steps, p); j > i {
				return fmt.Errorf("invalid config: step %q must come before \"encrypt\" because it needs the plaintext payload", p)
			}
		}
	}
	if i := slices.Index(steps, "decrypt"); i >= 0 {
		if j := slices.Index(steps, "validateSign"); j < 0 || j > i {
			return fmt.Errorf("invalid config: step \"validateSign\" must come before \"decrypt\" so the signature is verified over the received payload")
		}
		for _, p := range plaintextSteps {
			if j := slices.Index(steps, p); j >= 0 && j < i {
				return fmt.Errorf("invalid config: step %q must come after \"decrypt\" because it needs the plaintext payload", p)
			}
		}
	}
	return nil
}

// marshalJSON encodes v without HTML-escaping so payload strings containing
// '<', '>' or '&' round-trip byte-for-byte.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// decodeTopLevel decodes body into its top-level members.
func decodeTopLevel(step string, body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, model.NewBadReqErr("SCH_INVALID_JSON", fmt.Errorf("%s: failed to decode request body: %w", step, err))
	}
	return fields, nil
}

// ---------------------------------------------------------------------------
// encryptStep — caller (outbound) side
// ---------------------------------------------------------------------------

// encryptStep encrypts the outbound payload for the counterparty when the
// counterparty has published an encryption public key in the registry.
type encryptStep struct {
	encrypter definition.Encrypter
	km        definition.KeyManager
	registry  definition.RegistryLookup
	// payloadStore may be nil. When set, earlier messages of the same
	// transaction are used to learn which registry key the recipient signs
	// with, so the registry lookup can be keyed by key_id (required by
	// registries such as dediregistry).
	payloadStore definition.PayloadStore
}

// newEncryptStep validates dependencies and returns the encrypt step.
func newEncryptStep(encrypter definition.Encrypter, km definition.KeyManager, registry definition.RegistryLookup, payloadStore definition.PayloadStore) (definition.Step, error) {
	if encrypter == nil {
		return nil, fmt.Errorf("invalid config: Encrypter plugin not configured")
	}
	if km == nil {
		return nil, fmt.Errorf("invalid config: KeyManager plugin not configured")
	}
	if registry == nil {
		return nil, fmt.Errorf("invalid config: Registry plugin not configured")
	}
	return &encryptStep{encrypter: encrypter, km: km, registry: registry, payloadStore: payloadStore}, nil
}

// Run replaces every non-context member of the body with an encrypted
// envelope addressed to the counterparty, or leaves the body untouched when
// there is no single counterparty or it has not published an encryption key.
func (s *encryptStep) Run(ctx *model.StepContext) error {
	if len(ctx.Body) == 0 {
		return nil
	}
	fields, err := decodeTopLevel("encrypt", ctx.Body)
	if err != nil {
		return err
	}
	var reqContext map[string]any
	if err := json.Unmarshal(fields[becknContextKey], &reqContext); err != nil || reqContext == nil {
		return model.NewBadReqErr("SCH_REQUIRED_FIELD_MISSING", fmt.Errorf("encrypt: context field not found or invalid"))
	}
	if _, ok := fields[encryptedEnvelopeKey]; ok {
		return model.NewBadReqErr("", fmt.Errorf("encrypt: payload already carries an %q member", encryptedEnvelopeKey))
	}
	if len(fields) == 1 {
		log.Debugf(ctx, "encrypt: payload has nothing besides context; sending unencrypted")
		return nil
	}

	recipientID := model.ResolveCallerID(reqContext, ctx.Role)
	if recipientID == "" {
		// e.g. a broadcast search without bpp_id: there is no single
		// recipient whose key could be used.
		log.Debugf(ctx, "encrypt: no counterparty id in context for role %q; sending unencrypted", ctx.Role)
		return nil
	}
	if len(ctx.SubID) == 0 {
		return model.NewBadReqErr("", fmt.Errorf("subscriberID not set"))
	}

	recipient, err := s.lookupRecipient(ctx, recipientID, s.recipientKeyHint(ctx, reqContext, recipientID))
	if err != nil {
		if isClientCodedErr(err) {
			// The registry answered, but not with a usable key (e.g. not
			// found, or dediregistry needs a key id we don't know).
			// Encryption is opportunistic, so this is the same as a recipient
			// without a published key.
			log.Warnf(ctx, "encrypt: no encryption key could be determined for recipient %s, sending unencrypted: %v", recipientID, err)
			return nil
		}
		// Fail closed on an outage: the recipient may have published a key,
		// and silently downgrading to plaintext would defeat its opt-in.
		return fmt.Errorf("encrypt: failed to look up encryption key for recipient %s: %w", recipientID, err)
	}
	if recipient == nil {
		log.Debugf(ctx, "encrypt: recipient %s has not published an encryption public key; sending unencrypted", recipientID)
		return nil
	}

	keyset, err := s.km.Keyset(ctx, ctx.SubID)
	if err != nil {
		return fmt.Errorf("encrypt: failed to get encryption key: %w", err)
	}
	if keyset == nil || keyset.EncrPrivate == "" || keyset.UniqueKeyID == "" {
		return fmt.Errorf("encrypt: keyset for subscriber %s has no encryption private key or key id", ctx.SubID)
	}

	contextRaw := fields[becknContextKey]
	delete(fields, becknContextKey)
	plaintext, err := marshalJSON(fields)
	if err != nil {
		return fmt.Errorf("encrypt: failed to encode payload: %w", err)
	}
	ciphertext, err := s.encrypter.Encrypt(ctx, string(plaintext), keyset.EncrPrivate, recipient.EncrPublicKey)
	if err != nil {
		return encryptFailure(fmt.Sprintf("encrypt: failed to encrypt payload for recipient %s", recipientID), err)
	}
	envelope, err := marshalJSON(&encryptedEnvelope{
		Version:        encryptedEnvelopeVersion,
		SenderID:       ctx.SubID,
		SenderKeyID:    keyset.UniqueKeyID,
		RecipientID:    recipientID,
		RecipientKeyID: recipient.KeyID,
		Ciphertext:     ciphertext,
	})
	if err != nil {
		return fmt.Errorf("encrypt: failed to encode envelope: %w", err)
	}
	body, err := marshalJSON(map[string]json.RawMessage{
		becknContextKey:      contextRaw,
		encryptedEnvelopeKey: envelope,
	})
	if err != nil {
		return fmt.Errorf("encrypt: failed to encode request body: %w", err)
	}
	ctx.Body = body
	log.Debugf(ctx, "encrypt: payload encrypted for recipient %s (key %s)", recipientID, recipient.KeyID)
	return nil
}

// isClientCodedErr reports whether err carries a 4xx *model.CodedErr.
//
// For a registry lookup this separates the registry's answer about the
// looked-up subscriber from a failure to get an answer: registry plugins
// classify the former as 4xx (dediregistry: AUT_SUBSCRIBER_NOT_FOUND for a
// 404, AUT_SIGNATURE_INVALID for a missing key id, AUT_NETWORK_NOT_ALLOWED,
// ...) and outages as 5xx (unreachable, timeout, bad upstream response,
// cancelled). Unclassified errors, which is how the legacy registry plugin
// reports every failure, count as outages.
func isClientCodedErr(err error) bool {
	var coded *model.CodedErr
	return errors.As(err, &coded) && coded.HTTPStatus() < http.StatusInternalServerError
}

// encryptFailure wraps an error from the encrypter plugin. Its 4xx
// classifications describe a key (the recipient's published one or our own),
// not the request being sent, so returning them would tell the calling
// application its own request is malformed. They are reported unclassified
// (500, full detail in the log).
func encryptFailure(msg string, err error) error {
	if isClientCodedErr(err) {
		return fmt.Errorf("%s: %v", msg, err)
	}
	return fmt.Errorf("%s: %w", msg, err)
}

// lookupRecipient returns the first usable registry record of recipientID
// that carries an encryption public key, or nil when there is none (the
// recipient has not opted in). keyIDHint narrows the lookup when known.
func (s *encryptStep) lookupRecipient(ctx *model.StepContext, recipientID, keyIDHint string) (*model.Subscription, error) {
	records, err := s.registry.Lookup(ctx, &model.Subscription{
		Subscriber: model.Subscriber{SubscriberID: recipientID},
		KeyID:      keyIDHint,
	})
	if err != nil {
		return nil, err
	}
	for i := range records {
		r := &records[i]
		if r.SubscriberID != "" && r.SubscriberID != recipientID {
			continue
		}
		if r.EncrPublicKey == "" || !model.IsKeyStatusUsable(r.Status) {
			continue
		}
		return r, nil
	}
	return nil, nil
}

// recipientKeyHint returns the registry key id the recipient used to sign the
// most recent message of this transaction that the PayloadStore holds (e.g.
// the request a callback answers), or "" when unknown. A hint is optional:
// failures are logged and degrade to a subscriber-wide registry lookup.
func (s *encryptStep) recipientKeyHint(ctx *model.StepContext, reqContext map[string]any, recipientID string) string {
	if s.payloadStore == nil {
		return ""
	}
	var txnID string
	for _, k := range []string{"transaction_id", "transactionId"} {
		if v, ok := reqContext[k].(string); ok && v != "" {
			txnID = v
			break
		}
	}
	if txnID == "" {
		return ""
	}
	entries, err := s.payloadStore.GetByTransactionID(ctx, txnID)
	if err != nil {
		log.Warnf(ctx, "encrypt: PayloadStore lookup failed for transaction_id=%s: %v", txnID, err)
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		hdr, err := parseHeader(entries[i].Signature)
		if err != nil {
			continue
		}
		if hdr.SubscriberID == recipientID && hdr.UniqueID != "" {
			return hdr.UniqueID
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// decryptStep — receiver (inbound) side
// ---------------------------------------------------------------------------

// decryptStep restores the plaintext payload of an inbound message that
// carries an encrypted envelope. Messages without the envelope pass through.
type decryptStep struct {
	decrypter definition.Decrypter
	km        definition.KeyManager
}

// newDecryptStep validates dependencies and returns the decrypt step.
func newDecryptStep(decrypter definition.Decrypter, km definition.KeyManager) (definition.Step, error) {
	if decrypter == nil {
		return nil, fmt.Errorf("invalid config: Decrypter plugin not configured")
	}
	if km == nil {
		return nil, fmt.Errorf("invalid config: KeyManager plugin not configured")
	}
	return &decryptStep{decrypter: decrypter, km: km}, nil
}

// Run decrypts the envelope and rebuilds the original body around the
// plaintext context.
func (s *decryptStep) Run(ctx *model.StepContext) error {
	if len(ctx.Body) == 0 {
		return nil
	}
	fields, err := decodeTopLevel("decrypt", ctx.Body)
	if err != nil {
		return err
	}
	envelopeRaw, ok := fields[encryptedEnvelopeKey]
	if !ok {
		log.Debugf(ctx, "decrypt: payload is not encrypted; passing through")
		return nil
	}
	contextRaw, ok := fields[becknContextKey]
	if !ok || len(fields) != 2 {
		return model.NewBadReqErr("", fmt.Errorf("decrypt: an encrypted payload must contain exactly the %q and %q members", becknContextKey, encryptedEnvelopeKey))
	}
	var env encryptedEnvelope
	if err := json.Unmarshal(envelopeRaw, &env); err != nil {
		return model.NewBadReqErr("", fmt.Errorf("decrypt: invalid encrypted envelope: %w", err))
	}
	if err := env.validate(); err != nil {
		return model.NewBadReqErr("", fmt.Errorf("decrypt: %w", err))
	}
	if len(ctx.SubID) == 0 {
		return model.NewBadReqErr("", fmt.Errorf("subscriberID not set"))
	}
	if env.RecipientID != ctx.SubID {
		return model.NewBadReqErr("", fmt.Errorf("decrypt: payload is encrypted for %q, not for %q", env.RecipientID, ctx.SubID))
	}
	if err := checkEnvelopeSender(ctx, &env); err != nil {
		return err
	}

	keyset, err := s.km.Keyset(ctx, ctx.SubID)
	if err != nil {
		return fmt.Errorf("decrypt: failed to get decryption key: %w", err)
	}
	if keyset == nil || keyset.EncrPrivate == "" {
		return fmt.Errorf("decrypt: keyset for subscriber %s has no encryption private key", ctx.SubID)
	}
	if env.RecipientKeyID != "" && keyset.UniqueKeyID != "" && env.RecipientKeyID != keyset.UniqueKeyID {
		return model.NewBadReqErr("", fmt.Errorf("decrypt: payload is encrypted for key %q, but the local key is %q", env.RecipientKeyID, keyset.UniqueKeyID))
	}
	_, senderEncrPublic, err := s.km.LookupNPKeys(ctx, env.SenderID, env.SenderKeyID)
	if err != nil {
		return fmt.Errorf("decrypt: failed to look up encryption key of sender %s: %w", env.SenderID, err)
	}
	if senderEncrPublic == "" {
		return model.NewSignValidationErr("AUT_KEY_NOT_FOUND", fmt.Errorf("decrypt: sender %s has no encryption public key registered for key %s", env.SenderID, env.SenderKeyID))
	}

	plaintext, err := s.decrypter.Decrypt(ctx, env.Ciphertext, keyset.EncrPrivate, senderEncrPublic)
	if err != nil {
		return model.NewBadReqErr("", fmt.Errorf("decrypt: failed to decrypt payload: %w", err))
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(plaintext), &payload); err != nil || payload == nil {
		return model.NewBadReqErr("", fmt.Errorf("decrypt: decrypted payload is not a JSON object"))
	}
	if _, ok := payload[becknContextKey]; ok {
		// The plaintext context is what was signed and routed on; never let
		// the encrypted part override it.
		return model.NewBadReqErr("", fmt.Errorf("decrypt: decrypted payload must not contain %q", becknContextKey))
	}
	payload[becknContextKey] = contextRaw
	body, err := marshalJSON(payload)
	if err != nil {
		return fmt.Errorf("decrypt: failed to encode request body: %w", err)
	}
	ctx.Body = body
	log.Debugf(ctx, "decrypt: payload from %s decrypted", env.SenderID)
	return nil
}

// checkEnvelopeSender binds the envelope to the request signer: the
// Authorization keyId (subscriber and key) must be the envelope's sender and
// sender key. This stops a participant from replaying someone else's
// ciphertext under its own signature, and makes the sender's encryption key
// come from the same registry record validateSign authenticated. Signature
// validity is validateSign's job, which start-up guarantees runs first.
func checkEnvelopeSender(ctx *model.StepContext, env *encryptedEnvelope) error {
	header := ctx.Request.Header.Get(model.AuthHeaderSubscriber)
	if header == "" {
		return model.NewSignValidationErr("AUT_SIGNATURE_MISSING", fmt.Errorf("decrypt: encrypted payload has no %s header", model.AuthHeaderSubscriber))
	}
	parsed, err := parseHeader(header)
	if err != nil {
		return model.NewSignValidationErr("", fmt.Errorf("decrypt: %w", err))
	}
	if parsed.SubscriberID != env.SenderID || parsed.UniqueID != env.SenderKeyID {
		return model.NewSignValidationErr("", fmt.Errorf("decrypt: envelope sender %q (key %q) does not match signing subscriber %q (key %q)",
			env.SenderID, env.SenderKeyID, parsed.SubscriberID, parsed.UniqueID))
	}
	return nil
}
