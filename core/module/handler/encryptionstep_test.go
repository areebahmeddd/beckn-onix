package handler

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	decryptplugin "github.com/beckn-one/beckn-onix/pkg/plugin/implementation/decrypter"
	encryptplugin "github.com/beckn-one/beckn-onix/pkg/plugin/implementation/encrypter"
)

// ---------------------------------------------------------------------------
// Fakes and helpers
// ---------------------------------------------------------------------------

// encTestParticipant is a network participant with real signing (Ed25519) and
// encryption (X25519) key pairs.
type encTestParticipant struct {
	keyset *model.Keyset
	record model.Subscription
}

func newEncTestParticipant(t *testing.T, subscriberID, keyID string, publishEncrKey bool) encTestParticipant {
	t.Helper()
	signPub, signPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	encrPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("X25519 GenerateKey: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString
	p := encTestParticipant{
		keyset: &model.Keyset{
			SubscriberID:   subscriberID,
			UniqueKeyID:    keyID,
			SigningPrivate: b64(signPriv.Seed()),
			SigningPublic:  b64(signPub),
			EncrPrivate:    b64(encrPriv.Bytes()),
			EncrPublic:     b64(encrPriv.PublicKey().Bytes()),
		},
		record: model.Subscription{
			Subscriber:       model.Subscriber{SubscriberID: subscriberID},
			KeyID:            keyID,
			SigningPublicKey: b64(signPub),
			Status:           "SUBSCRIBED",
		},
	}
	if publishEncrKey {
		p.record.EncrPublicKey = p.keyset.EncrPublic
	}
	return p
}

// encTestRegistry is an in-memory RegistryLookup that records its queries.
type encTestRegistry struct {
	mu      sync.Mutex
	records []model.Subscription
	err     error
	queries []model.Subscription
}

func (r *encTestRegistry) Lookup(_ context.Context, req *model.Subscription) ([]model.Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, *req)
	if r.err != nil {
		return nil, r.err
	}
	var out []model.Subscription
	for _, rec := range r.records {
		if rec.SubscriberID == req.SubscriberID && (req.KeyID == "" || rec.KeyID == req.KeyID) {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (r *encTestRegistry) lastQuery(t *testing.T) model.Subscription {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queries) == 0 {
		t.Fatal("expected a registry lookup")
	}
	return r.queries[len(r.queries)-1]
}

// encTestKM is a KeyManager holding local keysets and resolving counterparty
// keys from an encTestRegistry, like simplekeymanager does.
type encTestKM struct {
	keysets   map[string]*model.Keyset
	registry  *encTestRegistry
	keysetErr error
	lookupErr error
}

func (k *encTestKM) GenerateKeyset() (*model.Keyset, error)                          { return nil, nil }
func (k *encTestKM) InsertKeyset(_ context.Context, _ string, _ *model.Keyset) error { return nil }
func (k *encTestKM) DeleteKeyset(_ context.Context, _ string) error                  { return nil }
func (k *encTestKM) Keyset(_ context.Context, id string) (*model.Keyset, error) {
	if k.keysetErr != nil {
		return nil, k.keysetErr
	}
	ks, ok := k.keysets[id]
	if !ok {
		return nil, errors.New("keyset not found")
	}
	cp := *ks
	return &cp, nil
}
func (k *encTestKM) LookupNPKeys(_ context.Context, subscriberID, keyID string) (string, string, error) {
	if k.lookupErr != nil {
		return "", "", k.lookupErr
	}
	k.registry.mu.Lock()
	defer k.registry.mu.Unlock()
	for _, rec := range k.registry.records {
		if rec.SubscriberID == subscriberID && rec.KeyID == keyID {
			return rec.SigningPublicKey, rec.EncrPublicKey, nil
		}
	}
	return "", "", model.NewSignValidationErr("AUT_SUBSCRIBER_NOT_FOUND", errors.New("no subscriber found"))
}

// stubEncrypter / stubDecrypter return fixed results for error-path tests.
type stubEncrypter struct {
	out string
	err error
}

func (s stubEncrypter) Encrypt(context.Context, string, string, string) (string, error) {
	return s.out, s.err
}

type stubDecrypter struct {
	out string
	err error
}

func (s stubDecrypter) Decrypt(context.Context, string, string, string) (string, error) {
	return s.out, s.err
}

// encTestPayloadStore serves GetByTransactionID from a fixed slice.
type encTestPayloadStore struct {
	mockPayloadStore
	entries []definition.PayloadEntry
	err     error
}

func (s *encTestPayloadStore) GetByTransactionID(context.Context, string) ([]definition.PayloadEntry, error) {
	return s.entries, s.err
}

const (
	encTestBAP = "bap.example.com"
	encTestBPP = "bpp.example.com"
)

// encTestBody is a v2 select from the BAP to the BPP. Its message contains
// characters json.Marshal would HTML-escape by default.
const encTestBody = `{"context":{"action":"select","version":"2.0.0","bapId":"bap.example.com","bppId":"bpp.example.com","transactionId":"txn-1","messageId":"msg-1"},"message":{"order":{"note":"<fish & chips>","items":[{"id":"i1","quantity":2}]}}}`

// encTestNetwork wires a BAP and a BPP that both publish encryption keys.
type encTestNetwork struct {
	bap, bpp encTestParticipant
	registry *encTestRegistry
	bapKM    *encTestKM
	bppKM    *encTestKM
}

func newEncTestNetwork(t *testing.T, bppPublishesEncrKey bool) *encTestNetwork {
	t.Helper()
	bap := newEncTestParticipant(t, encTestBAP, "bap-key-1", true)
	bpp := newEncTestParticipant(t, encTestBPP, "bpp-key-1", bppPublishesEncrKey)
	reg := &encTestRegistry{records: []model.Subscription{bap.record, bpp.record}}
	return &encTestNetwork{
		bap:      bap,
		bpp:      bpp,
		registry: reg,
		bapKM:    &encTestKM{keysets: map[string]*model.Keyset{encTestBAP: bap.keyset}, registry: reg},
		bppKM:    &encTestKM{keysets: map[string]*model.Keyset{encTestBPP: bpp.keyset}, registry: reg},
	}
}

// encTestAuthHeader returns an Authorization header whose keyId names the
// given subscriber and key.
func encTestAuthHeader(subscriberID, keyID string) string {
	return strings.Replace(providerInitiatedAuthHeader(subscriberID), "|key-1|", "|"+keyID+"|", 1)
}

func encTestStepCtx(role model.Role, subID, body string) *model.StepContext {
	req, _ := http.NewRequest(http.MethodPost, "/test/", strings.NewReader(body))
	return &model.StepContext{
		Context:    context.Background(),
		Request:    req,
		Body:       []byte(body),
		Role:       role,
		SubID:      subID,
		RespHeader: http.Header{},
	}
}

func realEncrypter(t *testing.T) definition.Encrypter {
	t.Helper()
	e, _, err := encryptplugin.New(context.Background())
	if err != nil {
		t.Fatalf("encrypter.New: %v", err)
	}
	return e
}

func realDecrypter(t *testing.T) definition.Decrypter {
	t.Helper()
	d, _, err := decryptplugin.New(context.Background())
	if err != nil {
		t.Fatalf("decrypter.New: %v", err)
	}
	return d
}

// mustEncrypt runs a real encrypt step for the BAP and returns the wire body.
func (n *encTestNetwork) mustEncrypt(t *testing.T, body string) []byte {
	t.Helper()
	step, err := newEncryptStep(realEncrypter(t), n.bapKM, n.registry, nil)
	if err != nil {
		t.Fatalf("newEncryptStep: %v", err)
	}
	ctx := encTestStepCtx(model.RoleBAP, encTestBAP, body)
	if err := step.Run(ctx); err != nil {
		t.Fatalf("encrypt Run: %v", err)
	}
	return ctx.Body
}

func decodeObject(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v\n%s", err, b)
	}
	return m
}

// assertJSONEqual compares two JSON documents semantically.
func assertJSONEqual(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	wb, _ := json.Marshal(w)
	gb, _ := json.Marshal(g)
	if string(wb) != string(gb) {
		t.Fatalf("JSON mismatch\nwant: %s\n got: %s", wb, gb)
	}
}

// assertCoded asserts err is (or wraps) a *model.CodedErr with the given
// HTTP status and, when code is non-empty, Beckn error code.
func assertCoded(t *testing.T, err error, status int, code string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var coded *model.CodedErr
	if !errors.As(err, &coded) {
		t.Fatalf("expected *model.CodedErr, got %T: %v", err, err)
	}
	if coded.HTTPStatus() != status {
		t.Errorf("status = %d, want %d (%v)", coded.HTTPStatus(), status, err)
	}
	if code != "" && coded.BecknError().Code != code {
		t.Errorf("code = %q, want %q (%v)", coded.BecknError().Code, code, err)
	}
}

// ---------------------------------------------------------------------------
// Constructors
// ---------------------------------------------------------------------------

func TestNewEncryptStep_MissingDependencies(t *testing.T) {
	km := &encTestKM{}
	reg := &encTestRegistry{}
	enc := stubEncrypter{}
	tests := []struct {
		name    string
		enc     definition.Encrypter
		km      definition.KeyManager
		reg     definition.RegistryLookup
		wantErr string
	}{
		{"no encrypter", nil, km, reg, "Encrypter plugin not configured"},
		{"no key manager", enc, nil, reg, "KeyManager plugin not configured"},
		{"no registry", enc, km, nil, "Registry plugin not configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newEncryptStep(tt.enc, tt.km, tt.reg, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
	if _, err := newEncryptStep(enc, km, reg, nil); err != nil {
		t.Fatalf("unexpected error with all dependencies: %v", err)
	}
}

func TestNewDecryptStep_MissingDependencies(t *testing.T) {
	if _, err := newDecryptStep(nil, &encTestKM{}); err == nil || !strings.Contains(err.Error(), "Decrypter plugin not configured") {
		t.Fatalf("err = %v, want Decrypter error", err)
	}
	if _, err := newDecryptStep(stubDecrypter{}, nil); err == nil || !strings.Contains(err.Error(), "KeyManager plugin not configured") {
		t.Fatalf("err = %v, want KeyManager error", err)
	}
	if _, err := newDecryptStep(stubDecrypter{}, &encTestKM{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// encryptStep
// ---------------------------------------------------------------------------

func TestEncryptStep_Run_RecipientHasKey_EncryptsForRecipient(t *testing.T) {
	n := newEncTestNetwork(t, true)
	wire := n.mustEncrypt(t, encTestBody)

	fields := decodeObject(t, wire)
	if len(fields) != 2 {
		t.Fatalf("wire body should contain only context and encrypted, got %s", wire)
	}
	original := decodeObject(t, []byte(encTestBody))
	if string(fields["context"]) != string(original["context"]) {
		t.Errorf("context must stay byte-identical plaintext\nwant %s\n got %s", original["context"], fields["context"])
	}
	if strings.Contains(string(wire), "fish") {
		t.Errorf("message content leaked into the wire body: %s", wire)
	}
	var env encryptedEnvelope
	if err := json.Unmarshal(fields["encrypted"], &env); err != nil {
		t.Fatalf("invalid envelope: %v", err)
	}
	want := encryptedEnvelope{
		Version:        encryptedEnvelopeVersion,
		SenderID:       encTestBAP,
		SenderKeyID:    "bap-key-1",
		RecipientID:    encTestBPP,
		RecipientKeyID: "bpp-key-1",
		Ciphertext:     env.Ciphertext,
	}
	if env != want || env.Ciphertext == "" {
		t.Errorf("envelope = %+v, want %+v", env, want)
	}
	if q := n.registry.lastQuery(t); q.SubscriberID != encTestBPP || q.KeyID != "" {
		t.Errorf("registry queried with %+v, want subscriber %s and no key hint", q, encTestBPP)
	}

	// The BPP can decrypt it with the real decrypter plugin.
	plaintext, err := realDecrypter(t).Decrypt(context.Background(), env.Ciphertext, n.bpp.keyset.EncrPrivate, n.bap.keyset.EncrPublic)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plaintext != `{"message":{"order":{"note":"<fish & chips>","items":[{"id":"i1","quantity":2}]}}}` {
		t.Errorf("plaintext = %s", plaintext)
	}
}

func TestEncryptStep_Run_CallbackFromBPP_EncryptsForBAP(t *testing.T) {
	n := newEncTestNetwork(t, true)
	step, _ := newEncryptStep(realEncrypter(t), n.bppKM, n.registry, nil)
	body := `{"context":{"action":"on_select","bap_id":"bap.example.com","bpp_id":"bpp.example.com"},"message":{"x":1},"error":{"code":"E1"}}`
	ctx := encTestStepCtx(model.RoleBPP, encTestBPP, body)
	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var env encryptedEnvelope
	if err := json.Unmarshal(decodeObject(t, ctx.Body)["encrypted"], &env); err != nil {
		t.Fatalf("invalid envelope: %v", err)
	}
	if env.RecipientID != encTestBAP || env.SenderID != encTestBPP {
		t.Errorf("envelope sender/recipient = %s/%s, want %s/%s", env.SenderID, env.RecipientID, encTestBPP, encTestBAP)
	}
	if strings.Contains(string(ctx.Body), `"E1"`) {
		t.Errorf("every non-context member, including error, must be encrypted: %s", ctx.Body)
	}
}

func TestEncryptStep_Run_PassThrough(t *testing.T) {
	expired := newEncTestParticipant(t, encTestBPP, "bpp-key-1", true)
	expired.record.Status = "EXPIRED"
	otherSubscriber := newEncTestParticipant(t, "someone-else.example.com", "bpp-key-1", true)

	tests := []struct {
		name string
		body string
		role model.Role
		// records replaces the registry contents when non-nil (default: a
		// BPP that publishes no encryption key).
		records []model.Subscription
		// anySubscriber makes the registry return records regardless of the
		// queried subscriber, exercising the step's own filtering.
		anySubscriber bool
		wantLookups   int
	}{
		{name: "empty body", body: ""},
		{name: "only context", body: `{"context":{"bppId":"bpp.example.com"}}`},
		{name: "no recipient id (broadcast search)", body: `{"context":{"action":"search","bapId":"bap.example.com"},"message":{}}`},
		{name: "gateway role has no single counterparty", body: encTestBody, role: model.RoleGateway},
		{name: "recipient publishes no encryption key", body: encTestBody, wantLookups: 1},
		{name: "recipient not in registry", body: encTestBody, records: []model.Subscription{}, wantLookups: 1},
		{name: "recipient key expired", body: encTestBody, records: []model.Subscription{expired.record}, wantLookups: 1},
		{name: "registry returns another subscriber", body: encTestBody, records: []model.Subscription{otherSubscriber.record}, anySubscriber: true, wantLookups: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newEncTestNetwork(t, false)
			reg := n.registry
			if tt.records != nil {
				reg = &encTestRegistry{records: tt.records}
			}
			var lookup definition.RegistryLookup = reg
			if tt.anySubscriber {
				lookup = registryFunc(func(_ context.Context, req *model.Subscription) ([]model.Subscription, error) {
					reg.queries = append(reg.queries, *req)
					return reg.records, nil
				})
			}
			role := tt.role
			if role == "" {
				role = model.RoleBAP
			}
			step, _ := newEncryptStep(stubEncrypter{err: errors.New("must not be called")}, n.bapKM, lookup, nil)
			ctx := encTestStepCtx(role, encTestBAP, tt.body)
			if err := step.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if string(ctx.Body) != tt.body {
				t.Errorf("body changed on pass-through:\nwant %s\n got %s", tt.body, ctx.Body)
			}
			if len(reg.queries) != tt.wantLookups {
				t.Errorf("registry lookups = %d, want %d", len(reg.queries), tt.wantLookups)
			}
		})
	}
}

// registryFunc adapts a function to definition.RegistryLookup.
type registryFunc func(context.Context, *model.Subscription) ([]model.Subscription, error)

func (f registryFunc) Lookup(ctx context.Context, req *model.Subscription) ([]model.Subscription, error) {
	return f(ctx, req)
}

func TestEncryptStep_Run_Errors(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		subID      string
		setup      func(n *encTestNetwork) (definition.Encrypter, definition.KeyManager)
		wantStatus int    // 0: unclassified error (500)
		wantCode   string // optional
		wantMsg    string
	}{
		{name: "invalid JSON", body: `{"context":`, wantStatus: http.StatusBadRequest, wantCode: "SCH_INVALID_JSON"},
		{name: "missing context", body: `{"message":{}}`, wantStatus: http.StatusBadRequest, wantCode: "SCH_REQUIRED_FIELD_MISSING"},
		{name: "context not an object", body: `{"context":"x","message":{}}`, wantStatus: http.StatusBadRequest, wantCode: "SCH_REQUIRED_FIELD_MISSING"},
		{name: "already encrypted", body: `{"context":{"bppId":"bpp.example.com"},"encrypted":{}}`, wantStatus: http.StatusBadRequest, wantMsg: "already carries"},
		{name: "subscriber id not set", body: encTestBody, subID: "-", wantStatus: http.StatusBadRequest, wantMsg: "subscriberID not set"},
		{
			name: "keyset lookup fails", body: encTestBody,
			setup: func(n *encTestNetwork) (definition.Encrypter, definition.KeyManager) {
				n.bapKM.keysetErr = errors.New("vault down")
				return realEncrypter(t), n.bapKM
			},
			wantMsg: "vault down",
		},
		{
			name: "keyset has no encryption key", body: encTestBody,
			setup: func(n *encTestNetwork) (definition.Encrypter, definition.KeyManager) {
				n.bapKM.keysets[encTestBAP].EncrPrivate = ""
				return realEncrypter(t), n.bapKM
			},
			wantMsg: "no encryption private key",
		},
		{
			name: "encrypter fails", body: encTestBody,
			setup: func(n *encTestNetwork) (definition.Encrypter, definition.KeyManager) {
				return stubEncrypter{err: errors.New("boom")}, n.bapKM
			},
			wantMsg: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newEncTestNetwork(t, true)
			var enc definition.Encrypter = realEncrypter(t)
			var km definition.KeyManager = n.bapKM
			if tt.setup != nil {
				enc, km = tt.setup(n)
			}
			subID := encTestBAP
			if tt.subID == "-" {
				subID = ""
			}
			step, _ := newEncryptStep(enc, km, n.registry, nil)
			ctx := encTestStepCtx(model.RoleBAP, subID, tt.body)
			err := step.Run(ctx)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tt.wantStatus != 0 {
				assertCoded(t, err, tt.wantStatus, tt.wantCode)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v, want it to mention %q", err, tt.wantMsg)
			}
			if string(ctx.Body) != tt.body {
				t.Errorf("body must be unchanged on error")
			}
		})
	}
}

func TestEncryptStep_Run_RegistryErrorFailsClosed(t *testing.T) {
	n := newEncTestNetwork(t, true)
	n.registry.err = model.NewCodedErr(http.StatusServiceUnavailable, "NET_DOWNSTREAM_UNAVAILABLE", errors.New("registry down"))
	step, _ := newEncryptStep(stubEncrypter{err: errors.New("must not be called")}, n.bapKM, n.registry, nil)
	ctx := encTestStepCtx(model.RoleBAP, encTestBAP, encTestBody)
	err := step.Run(ctx)
	// The registry's own 5xx classification is preserved.
	assertCoded(t, err, http.StatusServiceUnavailable, "NET_DOWNSTREAM_UNAVAILABLE")
	if string(ctx.Body) != encTestBody {
		t.Error("body must not be sent in plaintext form after a failed lookup")
	}
}

// A 4xx from the registry or encrypter describes the recipient's record or a
// key, not the caller's request, so it must surface as an unclassified 500
// rather than e.g. a 401 telling the application its own request is unsigned.
func TestEncryptStep_Run_ClientErrorsFromDependenciesAreNotForwarded(t *testing.T) {
	tests := []struct {
		name    string
		regErr  error
		enc     definition.Encrypter
		wantMsg string
	}{
		{
			name:    "registry rejects lookup without key id",
			regErr:  model.NewSignValidationErr("AUT_SIGNATURE_INVALID", errors.New("key_id is required for DeDi lookup")),
			enc:     stubEncrypter{err: errors.New("must not be called")},
			wantMsg: "key_id is required",
		},
		{
			name:    "recipient published an invalid key",
			enc:     stubEncrypter{err: model.NewBadReqErr("AUT_SIGNATURE_INVALID", errors.New("invalid public key"))},
			wantMsg: "invalid public key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newEncTestNetwork(t, true)
			n.registry.err = tt.regErr
			step, _ := newEncryptStep(tt.enc, n.bapKM, n.registry, nil)
			err := step.Run(encTestStepCtx(model.RoleBAP, encTestBAP, encTestBody))
			if err == nil {
				t.Fatal("expected an error")
			}
			var coded *model.CodedErr
			if errors.As(err, &coded) {
				t.Errorf("expected an unclassified (500) error, got %d %v", coded.HTTPStatus(), err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

func TestEncryptStep_RecipientKeyHint(t *testing.T) {
	bppAuth := providerInitiatedAuthHeader(encTestBPP) // keyId "bpp.example.com|key-1|ed25519"
	tests := []struct {
		name    string
		store   *encTestPayloadStore
		body    string
		wantKey string
	}{
		{name: "no payload store", body: encTestBody, wantKey: ""},
		{
			name: "uses most recent entry signed by the recipient",
			store: &encTestPayloadStore{entries: []definition.PayloadEntry{
				{Signature: strings.Replace(bppAuth, "key-1", "old-key", 1)},
				{Signature: bppAuth},
				{Signature: providerInitiatedAuthHeader(encTestBAP)},
				{Signature: ""},
			}},
			body: encTestBody, wantKey: "key-1",
		},
		{
			name:  "ignores entries signed by others",
			store: &encTestPayloadStore{entries: []definition.PayloadEntry{{Signature: providerInitiatedAuthHeader(encTestBAP)}}},
			body:  encTestBody, wantKey: "",
		},
		{
			name:  "store error degrades to no hint",
			store: &encTestPayloadStore{err: errors.New("redis down")},
			body:  encTestBody, wantKey: "",
		},
		{
			name:  "no transaction id",
			store: &encTestPayloadStore{entries: []definition.PayloadEntry{{Signature: bppAuth}}},
			body:  `{"context":{"bppId":"bpp.example.com"},"message":{}}`, wantKey: "",
		},
		{
			name:  "snake_case transaction id",
			store: &encTestPayloadStore{entries: []definition.PayloadEntry{{Signature: bppAuth}}},
			body:  `{"context":{"bpp_id":"bpp.example.com","transaction_id":"t"},"message":{}}`, wantKey: "key-1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &encTestRegistry{}
			var ps definition.PayloadStore
			if tt.store != nil {
				ps = tt.store
			}
			step, _ := newEncryptStep(stubEncrypter{}, &encTestKM{}, reg, ps)
			ctx := encTestStepCtx(model.RoleBAP, encTestBAP, tt.body)
			if err := step.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if q := reg.lastQuery(t); q.KeyID != tt.wantKey || q.SubscriberID != encTestBPP {
				t.Errorf("registry query = %+v, want subscriber %s key %q", q, encTestBPP, tt.wantKey)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// decryptStep
// ---------------------------------------------------------------------------

func TestDecryptStep_Run_RestoresPlaintext(t *testing.T) {
	n := newEncTestNetwork(t, true)
	wire := n.mustEncrypt(t, encTestBody)

	step, _ := newDecryptStep(realDecrypter(t), n.bppKM)
	ctx := encTestStepCtx(model.RoleBPP, encTestBPP, string(wire))
	ctx.Request.Header.Set(model.AuthHeaderSubscriber, encTestAuthHeader(encTestBAP, "bap-key-1"))
	if err := step.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertJSONEqual(t, []byte(encTestBody), ctx.Body)
	if !strings.Contains(string(ctx.Body), "<fish & chips>") {
		t.Errorf("decrypted body must not HTML-escape payload strings: %s", ctx.Body)
	}
}

func TestDecryptStep_Run_PassThrough(t *testing.T) {
	for _, body := range []string{"", encTestBody, `{"context":{},"message":{"encrypted":true}}`} {
		step, _ := newDecryptStep(stubDecrypter{err: errors.New("must not be called")}, &encTestKM{})
		ctx := encTestStepCtx(model.RoleBPP, encTestBPP, body)
		if err := step.Run(ctx); err != nil {
			t.Fatalf("Run(%q): %v", body, err)
		}
		if string(ctx.Body) != body {
			t.Errorf("body changed on pass-through: %s", ctx.Body)
		}
	}
}

func TestDecryptStep_Run_Errors(t *testing.T) {
	n := newEncTestNetwork(t, true)
	wire := string(n.mustEncrypt(t, encTestBody))
	withEnvelope := func(mutate func(e *encryptedEnvelope)) string {
		fields := decodeObject(t, []byte(wire))
		var env encryptedEnvelope
		_ = json.Unmarshal(fields["encrypted"], &env)
		mutate(&env)
		fields["encrypted"], _ = json.Marshal(env)
		b, _ := json.Marshal(fields)
		return string(b)
	}

	tests := []struct {
		name       string
		body       string
		subID      string
		auth       string // "": the BAP's own header; "-": no header
		km         func() definition.KeyManager
		decrypter  definition.Decrypter
		wantStatus int // 0: unclassified (500)
		wantCode   string
		wantMsg    string
	}{
		{name: "invalid JSON", body: `{`, wantStatus: http.StatusBadRequest, wantCode: "SCH_INVALID_JSON"},
		{name: "envelope without context", body: `{"encrypted":{}}`, wantStatus: http.StatusBadRequest, wantMsg: "exactly"},
		{name: "envelope next to plaintext members", body: `{"context":{},"encrypted":{},"message":{}}`, wantStatus: http.StatusBadRequest, wantMsg: "exactly"},
		{name: "envelope not an object", body: `{"context":{},"encrypted":"abc"}`, wantStatus: http.StatusBadRequest, wantMsg: "invalid encrypted envelope"},
		{name: "unsupported version", body: withEnvelope(func(e *encryptedEnvelope) { e.Version = "v9" }), wantStatus: http.StatusBadRequest, wantMsg: "unsupported encrypted envelope version"},
		{name: "missing senderId", body: withEnvelope(func(e *encryptedEnvelope) { e.SenderID = "" }), wantStatus: http.StatusBadRequest, wantMsg: "senderId"},
		{name: "missing senderKeyId", body: withEnvelope(func(e *encryptedEnvelope) { e.SenderKeyID = "" }), wantStatus: http.StatusBadRequest, wantMsg: "senderKeyId"},
		{name: "missing recipientId", body: withEnvelope(func(e *encryptedEnvelope) { e.RecipientID = "" }), wantStatus: http.StatusBadRequest, wantMsg: "recipientId"},
		{name: "missing ciphertext", body: withEnvelope(func(e *encryptedEnvelope) { e.Ciphertext = "" }), wantStatus: http.StatusBadRequest, wantMsg: "ciphertext"},
		{name: "subscriber id not set", body: wire, subID: "-", wantStatus: http.StatusBadRequest, wantMsg: "subscriberID not set"},
		{name: "addressed to someone else", body: withEnvelope(func(e *encryptedEnvelope) { e.RecipientID = "other.example.com" }), wantStatus: http.StatusBadRequest, wantMsg: "encrypted for"},
		{name: "no Authorization header", body: wire, auth: "-", wantStatus: http.StatusUnauthorized, wantCode: "AUT_SIGNATURE_MISSING"},
		{name: "signer is not the envelope sender", body: wire, auth: encTestAuthHeader("mallory.example.com", "bap-key-1"), wantStatus: http.StatusUnauthorized, wantMsg: "does not match signing subscriber"},
		{name: "signer key is not the envelope sender key", body: wire, auth: encTestAuthHeader(encTestBAP, "other-key"), wantStatus: http.StatusUnauthorized, wantMsg: "does not match signing subscriber"},
		{name: "unparseable Authorization header", body: wire, auth: "Signature nonsense", wantStatus: http.StatusUnauthorized},
		{
			name: "keyset lookup fails", body: wire,
			km:      func() definition.KeyManager { n.bppKM.keysetErr = errors.New("vault down"); return n.bppKM },
			wantMsg: "vault down",
		},
		{
			name: "keyset has no encryption key", body: wire,
			km: func() definition.KeyManager {
				return &encTestKM{keysets: map[string]*model.Keyset{encTestBPP: {UniqueKeyID: "bpp-key-1"}}, registry: n.registry}
			},
			wantMsg: "no encryption private key",
		},
		{name: "recipient key mismatch", body: withEnvelope(func(e *encryptedEnvelope) { e.RecipientKeyID = "rotated-key" }), wantStatus: http.StatusBadRequest, wantMsg: "encrypted for key"},
		{
			name: "sender lookup fails", body: wire,
			km: func() definition.KeyManager {
				return &encTestKM{keysets: n.bppKM.keysets, registry: n.registry, lookupErr: model.NewSignValidationErr("AUT_SUBSCRIBER_NOT_FOUND", errors.New("nope"))}
			},
			wantStatus: http.StatusUnauthorized, wantCode: "AUT_SUBSCRIBER_NOT_FOUND",
		},
		{
			name: "sender has no encryption key", body: wire,
			km: func() definition.KeyManager {
				bap := n.bap.record
				bap.EncrPublicKey = ""
				return &encTestKM{keysets: n.bppKM.keysets, registry: &encTestRegistry{records: []model.Subscription{bap}}}
			},
			wantStatus: http.StatusUnauthorized, wantCode: "AUT_KEY_NOT_FOUND",
		},
		{name: "tampered ciphertext", body: withEnvelope(func(e *encryptedEnvelope) { e.Ciphertext = "AAAA" + e.Ciphertext[4:] }), wantStatus: http.StatusBadRequest},
		{name: "ciphertext not base64", body: withEnvelope(func(e *encryptedEnvelope) { e.Ciphertext = "!!!" }), wantStatus: http.StatusBadRequest, wantMsg: "failed to decrypt"},
		{name: "plaintext not an object", body: wire, decrypter: stubDecrypter{out: `[1,2]`}, wantStatus: http.StatusBadRequest, wantMsg: "not a JSON object"},
		{name: "plaintext overrides context", body: wire, decrypter: stubDecrypter{out: `{"context":{"bapId":"evil"}}`}, wantStatus: http.StatusBadRequest, wantMsg: "must not contain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n.bppKM.keysetErr = nil
			var km definition.KeyManager = n.bppKM
			if tt.km != nil {
				km = tt.km()
			}
			dec := tt.decrypter
			if dec == nil {
				dec = realDecrypter(t)
			}
			subID := encTestBPP
			if tt.subID == "-" {
				subID = ""
			}
			step, _ := newDecryptStep(dec, km)
			ctx := encTestStepCtx(model.RoleBPP, subID, tt.body)
			switch tt.auth {
			case "-":
			case "":
				ctx.Request.Header.Set(model.AuthHeaderSubscriber, encTestAuthHeader(encTestBAP, "bap-key-1"))
			default:
				ctx.Request.Header.Set(model.AuthHeaderSubscriber, tt.auth)
			}
			err := step.Run(ctx)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tt.wantStatus != 0 {
				assertCoded(t, err, tt.wantStatus, tt.wantCode)
			} else {
				var coded *model.CodedErr
				if errors.As(err, &coded) {
					t.Errorf("expected an unclassified (500) error, got %v", err)
				}
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v, want it to mention %q", err, tt.wantMsg)
			}
			if string(ctx.Body) != tt.body {
				t.Error("body must be unchanged on error")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Step ordering
// ---------------------------------------------------------------------------

func TestValidateEncryptionStepOrder(t *testing.T) {
	tests := []struct {
		name    string
		steps   []string
		wantErr string
	}{
		{name: "no encryption steps", steps: []string{"sign", "validateSchema"}},
		{name: "caller order", steps: []string{"validateSchema", "checkPolicy", "transformPayload", "addRoute", "encrypt", "sign", "storePayload"}},
		{name: "receiver order", steps: []string{"validateSign", "decrypt", "mediateSchema", "validateSchema", "checkPolicy", "addRoute", "signAck"}},
		{name: "encrypt without sign", steps: []string{"addRoute", "encrypt"}, wantErr: `"encrypt" must be followed by "sign"`},
		{name: "decrypt without validateSign", steps: []string{"decrypt", "addRoute"}, wantErr: `"validateSign" must come before "decrypt"`},
		{name: "encrypt after sign", steps: []string{"sign", "encrypt"}, wantErr: `"encrypt" must be followed by "sign"`},
		{name: "schema validation after encrypt", steps: []string{"encrypt", "validateSchema", "sign"}, wantErr: `"validateSchema" must come before "encrypt"`},
		{name: "decrypt before validateSign", steps: []string{"decrypt", "validateSign"}, wantErr: `"validateSign" must come before "decrypt"`},
		{name: "policy check before decrypt", steps: []string{"validateSign", "checkPolicy", "decrypt"}, wantErr: `"checkPolicy" must come after "decrypt"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEncryptionStepOrder(tt.steps)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestMarshalJSON_Error(t *testing.T) {
	if _, err := marshalJSON(make(chan int)); err == nil {
		t.Fatal("expected an error for an unencodable value")
	}
}
