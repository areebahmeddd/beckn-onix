package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/signer"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/signvalidator"
)

// encE2EPluginManager hands real signer/signvalidator/encrypter/decrypter
// plugins and in-memory registry/keymanager/router fakes to NewStdHandler.
type encE2EPluginManager struct {
	noopPluginManager
	registry  definition.RegistryLookup
	km        definition.KeyManager
	signer    definition.Signer
	validator definition.SignValidator
	router    definition.Router
	encrypter definition.Encrypter
	decrypter definition.Decrypter
}

func (m encE2EPluginManager) Registry(context.Context, definition.Cache, *plugin.Config) (definition.RegistryLookup, error) {
	return m.registry, nil
}
func (m encE2EPluginManager) KeyManager(context.Context, definition.RegistryLookup, *plugin.Config) (definition.KeyManager, error) {
	return m.km, nil
}
func (m encE2EPluginManager) Signer(context.Context, *plugin.Config) (definition.Signer, error) {
	return m.signer, nil
}
func (m encE2EPluginManager) SignValidator(context.Context, *plugin.Config) (definition.SignValidator, error) {
	return m.validator, nil
}
func (m encE2EPluginManager) Router(context.Context, *plugin.Config) (definition.Router, error) {
	return m.router, nil
}
func (m encE2EPluginManager) Encryptor(context.Context, *plugin.Config) (definition.Encrypter, error) {
	return m.encrypter, nil
}
func (m encE2EPluginManager) Decryptor(context.Context, *plugin.Config) (definition.Decrypter, error) {
	return m.decrypter, nil
}

// staticURLRouter routes every request to a fixed URL.
type staticURLRouter struct{ target *url.URL }

func (r staticURLRouter) Route(context.Context, *url.URL, []byte) (*model.Route, error) {
	return &model.Route{TargetType: "url", URL: r.target}, nil
}

// bodyRecorder captures the last request body an http.Handler saw.
type bodyRecorder struct {
	mu    sync.Mutex
	body  []byte
	calls int
}

func (b *bodyRecorder) record(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.body, b.calls = body, b.calls+1
	b.mu.Unlock()
	return body
}

func (b *bodyRecorder) get() ([]byte, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.body, b.calls
}

// tamperCiphertext corrupts the envelope's ciphertext in an encrypted body.
func tamperCiphertext(t *testing.T, body []byte) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("wire body is not JSON: %v", err)
	}
	var env encryptedEnvelope
	if err := json.Unmarshal(fields[encryptedEnvelopeKey], &env); err != nil {
		t.Fatalf("wire body has no envelope: %v", err)
	}
	if strings.HasPrefix(env.Ciphertext, "AAAA") {
		env.Ciphertext = "BBBB" + env.Ciphertext[4:]
	} else {
		env.Ciphertext = "AAAA" + env.Ciphertext[4:]
	}
	fields[encryptedEnvelopeKey], _ = json.Marshal(env)
	out, _ := json.Marshal(fields)
	return out
}

// TestEncryption_RoundTrip_BAPToBPP sends a request through a real BAP caller
// handler (addRoute, encrypt, sign) to a real BPP receiver handler
// (validateSign, decrypt, addRoute) and on to a backend, and checks what was
// on the wire and what the backend received.
func TestEncryption_RoundTrip_BAPToBPP(t *testing.T) {
	tests := []struct {
		name               string
		bppPublishesKey    bool
		callerEncrypts     bool
		receiverDecrypts   bool
		tamper             bool
		registryErr        error
		wantStatus         int
		wantNoWire         bool
		wantWireEncrypted  bool
		wantBackendReached bool
	}{
		{
			name:            "both sides opted in: encrypted on the wire, plaintext at the backend",
			bppPublishesKey: true, callerEncrypts: true, receiverDecrypts: true,
			wantStatus: http.StatusOK, wantWireEncrypted: true, wantBackendReached: true,
		},
		{
			name:            "recipient has no encryption key: sent in plaintext",
			bppPublishesKey: false, callerEncrypts: true, receiverDecrypts: true,
			wantStatus: http.StatusOK, wantWireEncrypted: false, wantBackendReached: true,
		},
		{
			name:            "sender does not encrypt: receiver passes plaintext through",
			bppPublishesKey: true, callerEncrypts: false, receiverDecrypts: true,
			wantStatus: http.StatusOK, wantWireEncrypted: false, wantBackendReached: true,
		},
		{
			name:            "ciphertext tampered in transit: rejected by signature validation",
			bppPublishesKey: true, callerEncrypts: true, receiverDecrypts: true, tamper: true,
			wantStatus: http.StatusUnauthorized, wantWireEncrypted: true, wantBackendReached: false,
		},
		{
			name:            "registry outage: fails closed before anything is sent",
			bppPublishesKey: true, callerEncrypts: true, receiverDecrypts: true,
			registryErr: model.NewCodedErr(http.StatusServiceUnavailable, "NET_DOWNSTREAM_UNAVAILABLE", errors.New("registry down")),
			wantStatus:  http.StatusServiceUnavailable, wantNoWire: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			n := newEncTestNetwork(t, tt.bppPublishesKey)
			n.registry.err = tt.registryErr // encTestKM reads records directly, so validateSign is unaffected
			sgn, _, err := signer.New(ctx, &signer.Config{})
			if err != nil {
				t.Fatalf("signer.New: %v", err)
			}
			sv, _, err := signvalidator.New(ctx, &signvalidator.Config{})
			if err != nil {
				t.Fatalf("signvalidator.New: %v", err)
			}

			// BPP backend.
			backendSeen := &bodyRecorder{}
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backendSeen.record(r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"message":{"status":"ACK","messageId":"msg-1"}}`))
			}))
			defer backend.Close()
			backendURL, _ := url.Parse(backend.URL)

			// BPP receiver adapter.
			receiverCfg := &Config{
				Role:         model.RoleBPP,
				SubscriberID: encTestBPP,
				Plugins: PluginCfg{
					Registry:      cfg("registry"),
					KeyManager:    cfg("keymanager"),
					SignValidator: cfg("signvalidator"),
					Router:        cfg("router"),
				},
				Steps: []string{"validateSign", "addRoute"},
			}
			if tt.receiverDecrypts {
				receiverCfg.Plugins.Decrypter = cfg("decrypter")
				receiverCfg.Steps = []string{"validateSign", "decrypt", "addRoute"}
			}
			receiver, err := NewStdHandler(ctx, encE2EPluginManager{
				registry: n.registry, km: n.bppKM, validator: sv,
				router: staticURLRouter{target: backendURL}, decrypter: realDecrypter(t),
			}, receiverCfg, "bppTxnReceiver")
			if err != nil {
				t.Fatalf("receiver NewStdHandler: %v", err)
			}
			wireSeen := &bodyRecorder{}
			network := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := wireSeen.record(r)
				if tt.tamper {
					body = tamperCiphertext(t, body)
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
				receiver.ServeHTTP(w, r)
			}))
			defer network.Close()
			networkURL, _ := url.Parse(network.URL + "/bpp/receiver/select")

			// BAP caller adapter.
			callerCfg := &Config{
				Role:         model.RoleBAP,
				SubscriberID: encTestBAP,
				Plugins: PluginCfg{
					Registry:   cfg("registry"),
					KeyManager: cfg("keymanager"),
					Signer:     cfg("signer"),
					Router:     cfg("router"),
				},
				Steps: []string{"addRoute", "sign"},
			}
			if tt.callerEncrypts {
				callerCfg.Plugins.Encrypter = cfg("encrypter")
				callerCfg.Steps = []string{"addRoute", "encrypt", "sign"}
			}
			caller, err := NewStdHandler(ctx, encE2EPluginManager{
				registry: n.registry, km: n.bapKM, signer: sgn,
				router: staticURLRouter{target: networkURL}, encrypter: realEncrypter(t),
			}, callerCfg, "bapTxnCaller")
			if err != nil {
				t.Fatalf("caller NewStdHandler: %v", err)
			}

			rec := httptest.NewRecorder()
			caller.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/bap/caller/select", strings.NewReader(encTestBody)))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			wire, wireCalls := wireSeen.get()
			if tt.wantNoWire {
				if wireCalls != 0 {
					t.Fatalf("nothing may be sent, got %s", wire)
				}
				return
			}
			if wireCalls != 1 {
				t.Fatalf("expected exactly one request on the wire, got %d", wireCalls)
			}
			_, hasEnvelope := decodeObject(t, wire)[encryptedEnvelopeKey]
			if hasEnvelope != tt.wantWireEncrypted {
				t.Fatalf("wire body encrypted = %v, want %v: %s", hasEnvelope, tt.wantWireEncrypted, wire)
			}
			if tt.wantWireEncrypted && strings.Contains(string(wire), "fish") {
				t.Errorf("plaintext message content leaked onto the wire: %s", wire)
			}
			if !tt.wantWireEncrypted && string(wire) != encTestBody {
				t.Errorf("unencrypted wire body must be the original bytes\nwant %s\n got %s", encTestBody, wire)
			}

			backendBody, backendCalls := backendSeen.get()
			if !tt.wantBackendReached {
				if backendCalls != 0 {
					t.Fatalf("backend must not be reached, got body %s", backendBody)
				}
				return
			}
			if backendCalls != 1 {
				t.Fatalf("backend calls = %d, want 1", backendCalls)
			}
			assertJSONEqual(t, []byte(encTestBody), backendBody)
		})
	}
}

func TestNewStdHandler_EncryptionConfigErrors(t *testing.T) {
	ctx := context.Background()
	n := newEncTestNetwork(t, true)
	sgn, _, err := signer.New(ctx, &signer.Config{})
	if err != nil {
		t.Fatalf("signer.New: %v", err)
	}
	sv, _, err := signvalidator.New(ctx, &signvalidator.Config{})
	if err != nil {
		t.Fatalf("signvalidator.New: %v", err)
	}
	mgr := encE2EPluginManager{registry: n.registry, km: n.bapKM, signer: sgn, validator: sv, encrypter: stubEncrypter{}, decrypter: stubDecrypter{}}
	base := PluginCfg{Registry: cfg("registry"), KeyManager: cfg("km"), Signer: cfg("signer"), SignValidator: cfg("signvalidator")}
	with := func(mutate func(p *PluginCfg)) PluginCfg {
		p := base
		mutate(&p)
		return p
	}
	tests := []struct {
		name    string
		plugins PluginCfg
		steps   []string
		wantErr string
	}{
		{
			name:    "encrypt without encrypter plugin",
			plugins: base,
			steps:   []string{"encrypt", "sign"},
			wantErr: "Encrypter plugin not configured",
		},
		{
			name:    "decrypt without decrypter plugin",
			plugins: base,
			steps:   []string{"validateSign", "decrypt"},
			wantErr: "Decrypter plugin not configured",
		},
		{
			name:    "encrypt after sign",
			plugins: with(func(p *PluginCfg) { p.Encrypter = cfg("encrypter") }),
			steps:   []string{"sign", "encrypt"},
			wantErr: `"encrypt" must be followed by "sign"`,
		},
		{
			name:    "valid encrypt and decrypt",
			plugins: with(func(p *PluginCfg) { p.Encrypter, p.Decrypter = cfg("encrypter"), cfg("decrypter") }),
			steps:   []string{"encrypt", "sign", "validateSign", "decrypt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewStdHandler(ctx, mgr, &Config{Plugins: tt.plugins, Steps: tt.steps, Role: model.RoleBAP}, "test")
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
