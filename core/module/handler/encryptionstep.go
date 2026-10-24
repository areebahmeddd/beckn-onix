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

// Every top-level body member except "context" (which routing, signature
// checks and telemetry read) is encrypted into one "encrypted" member. The
// ciphertext has no integrity of its own, so "encrypt" runs before "sign" and
// "decrypt" after "validateSign": the signature covers the envelope.
const (
	encryptedEnvelopeKey     = "encrypted"
	encryptedEnvelopeVersion = "onix-v1"
	becknContextKey          = "context"
)

// encryptedEnvelope is the value of the "encrypted" body member. The sender
// key id lets the recipient find the sender's public key (static-static ECDH).
type encryptedEnvelope struct {
	Version        string `json:"version"`
	SenderID       string `json:"senderId"`
	SenderKeyID    string `json:"senderKeyId"`
	RecipientID    string `json:"recipientId"`
	RecipientKeyID string `json:"recipientKeyId,omitempty"`
	Ciphertext     string `json:"ciphertext"`
}

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

// plaintextSteps read the payload, so they must run before "encrypt" and after "decrypt".
var plaintextSteps = []string{"validateSchema", "checkPolicy", "mediateSchema", "transformPayload"}

// validateEncryptionStepOrder enforces encrypt-then-sign and keeps plaintextSteps
// on the plaintext side of "encrypt" and "decrypt".
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

func decodeTopLevel(step string, body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, model.NewBadReqErr("SCH_INVALID_JSON", fmt.Errorf("%s: failed to decode request body: %w", step, err))
	}
	return fields, nil
}

// encryptStep encrypts the outbound payload for a counterparty that has
// published an encryption public key in the registry.
type encryptStep struct {
	encrypter definition.Encrypter
	km        definition.KeyManager
	registry  definition.RegistryLookup
	// payloadStore may be nil; earlier messages of the transaction give the
	// recipient's key_id, which dediregistry lookups require.
	payloadStore definition.PayloadStore
}

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

// Run replaces every non-context member of the body with an envelope for the
// counterparty, or leaves the body as is when there is no key to encrypt for.
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
		log.Debugf(ctx, "encrypt: no counterparty id in context for role %q; sending unencrypted", ctx.Role)
		return nil
	}
	if len(ctx.SubID) == 0 {
		return model.NewBadReqErr("", fmt.Errorf("subscriberID not set"))
	}

	recipient, err := s.lookupRecipient(ctx, recipientID, s.recipientKeyHint(ctx, reqContext, recipientID))
	if err != nil {
		if isClientCodedErr(err) {
			// A definitive registry answer without a usable key: same as no published key.
			log.Warnf(ctx, "encrypt: no encryption key could be determined for recipient %s, sending unencrypted: %v", recipientID, err)
			return nil
		}
		// Fail closed on an outage: the recipient may have a key, and
		// downgrading to plaintext would defeat its opt-in.
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

// isClientCodedErr reports whether err carries a 4xx *model.CodedErr. Registry
// plugins use 4xx for answers about the subscriber; 5xx and unclassified errors
// (the legacy registry plugin) are outages.
func isClientCodedErr(err error) bool {
	var coded *model.CodedErr
	return errors.As(err, &coded) && coded.HTTPStatus() < http.StatusInternalServerError
}

// encryptFailure drops a 4xx classification from an encrypter error: it is
// about a key, not the caller's request, so it must not surface as a 400.
func encryptFailure(msg string, err error) error {
	if isClientCodedErr(err) {
		return fmt.Errorf("%s: %v", msg, err)
	}
	return fmt.Errorf("%s: %w", msg, err)
}

// lookupRecipient returns the first usable record of recipientID with an
// encryption public key, or nil when the recipient has not opted in.
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

// recipientKeyHint returns the key id the recipient signed its latest stored
// message of this transaction with, or "" when unknown.
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

// decryptStep restores the plaintext of an inbound encrypted payload.
// Messages without the envelope pass through.
type decryptStep struct {
	decrypter definition.Decrypter
	km        definition.KeyManager
}

func newDecryptStep(decrypter definition.Decrypter, km definition.KeyManager) (definition.Step, error) {
	if decrypter == nil {
		return nil, fmt.Errorf("invalid config: Decrypter plugin not configured")
	}
	if km == nil {
		return nil, fmt.Errorf("invalid config: KeyManager plugin not configured")
	}
	return &decryptStep{decrypter: decrypter, km: km}, nil
}

// Run decrypts the envelope and rebuilds the body around the plaintext context.
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
		// The plaintext context was signed and routed on; the ciphertext must not override it.
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

// checkEnvelopeSender requires the Authorization keyId to name the envelope's
// sender and key, so no one can re-sign another participant's ciphertext, and
// the sender's encryption key comes from the record validateSign authenticated.
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
