package handler

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// subscribingRegistry is a RegistryLookup that also implements
// RegistrySubscriber, recording what it was asked to publish.
type subscribingRegistry struct {
	registryWithoutMetadata
	result *model.SubscribeResult
	err    error
	calls  []*model.Subscription
}

func (r *subscribingRegistry) Subscribe(_ context.Context, req *model.Subscription) (*model.SubscribeResult, error) {
	r.calls = append(r.calls, req)
	return r.result, r.err
}

func subscribeTestKM() *mockKM {
	return &mockKM{keyset: &model.Keyset{
		SubscriberID:  "bap.example.com",
		UniqueKeyID:   "rec-id-1",
		SigningPublic: "signing-public",
		EncrPublic:    "encr-public",
	}}
}

func subscribeTestConfig() *Config {
	return &Config{
		Role:         model.RoleBAP,
		SubscriberID: "bap.example.com",
		Plugins:      PluginCfg{Registry: &plugin.Config{ID: "dediregistry"}},
		Subscribe:    &SubscribeConfig{URL: "https://bap.example.com/bap/receiver"},
	}
}

func TestSubscribeToRegistry_NotConfiguredIsNoop(t *testing.T) {
	cfg := subscribeTestConfig()
	cfg.Subscribe = nil
	// No registry or key manager: without a subscribe block nothing is checked.
	if err := subscribeToRegistry(context.Background(), nil, nil, cfg); err != nil {
		t.Fatalf("subscribeToRegistry() error = %v", err)
	}
}

func TestSubscribeToRegistry_PublishesKeysetAndCallback(t *testing.T) {
	tests := []struct {
		name     string
		role     model.Role
		typ      string
		wantType string
	}{
		{"bap role", model.RoleBAP, "", "BAP"},
		{"bpp role", model.RoleBPP, "", "BPP"},
		{"explicit type", model.RoleBPP, "CDS", "CDS"},
		{"explicit type for another role", model.RoleDiscovery, "DS", "DS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &subscribingRegistry{result: &model.SubscribeResult{Status: model.SubscribeCreated, RecordID: "rec-id-1", State: "live"}}
			cfg := subscribeTestConfig()
			cfg.Role = tt.role
			cfg.Subscribe.Type = tt.typ

			if err := subscribeToRegistry(context.Background(), reg, subscribeTestKM(), cfg); err != nil {
				t.Fatalf("subscribeToRegistry() error = %v", err)
			}
			if len(reg.calls) != 1 {
				t.Fatalf("expected 1 Subscribe call, got %d", len(reg.calls))
			}
			want := &model.Subscription{
				Subscriber: model.Subscriber{
					SubscriberID: "bap.example.com",
					URL:          "https://bap.example.com/bap/receiver",
					Type:         tt.wantType,
				},
				KeyID:            "rec-id-1",
				SigningPublicKey: "signing-public",
				EncrPublicKey:    "encr-public",
			}
			if got := reg.calls[0]; !reflect.DeepEqual(got, want) {
				t.Errorf("Subscribe(%+v), want %+v", got, want)
			}
		})
	}
}

func TestSubscribeToRegistry_InvalidConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		registry definition.RegistryLookup
		km       definition.KeyManager
		mutate   func(*Config)
		wantErr  string
	}{
		{"no registry", nil, subscribeTestKM(), nil, "requires a registry plugin"},
		{"registry cannot subscribe", registryWithoutMetadata{}, subscribeTestKM(), nil, "does not support subscribe"},
		{"no key manager", &subscribingRegistry{}, nil, nil, "requires a keyManager plugin"},
		{"no subscriberId", &subscribingRegistry{}, subscribeTestKM(), func(c *Config) { c.SubscriberID = "" }, "requires subscriberId"},
		{"no callback url", &subscribingRegistry{}, subscribeTestKM(), func(c *Config) { c.Subscribe.URL = "" }, "subscribe.url must be an absolute http(s) URL"},
		{"relative callback url", &subscribingRegistry{}, subscribeTestKM(), func(c *Config) { c.Subscribe.URL = "/bap/receiver" }, "subscribe.url must be an absolute http(s) URL"},
		{"no type for role", &subscribingRegistry{}, subscribeTestKM(), func(c *Config) { c.Role = model.RoleGateway }, `subscribe.type is required for role "gateway"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := subscribeTestConfig()
			cfg.Subscribe.Required = false // configuration errors fail regardless
			if tt.mutate != nil {
				tt.mutate(cfg)
			}
			err := subscribeToRegistry(context.Background(), tt.registry, tt.km, cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("subscribeToRegistry() error = %v, want it to contain %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "invalid subscribe configuration") {
				t.Errorf("error %q should be reported as a configuration error", err)
			}
			if reg, ok := tt.registry.(*subscribingRegistry); ok && len(reg.calls) != 0 {
				t.Error("Subscribe must not be called for an invalid configuration")
			}
		})
	}
}

func TestSubscribeToRegistry_RuntimeFailures(t *testing.T) {
	tests := []struct {
		name    string
		reg     *subscribingRegistry
		km      *mockKM
		wantErr string
	}{
		{"registry error", &subscribingRegistry{err: errors.New("registry down")}, subscribeTestKM(), "registry down"},
		{"no result", &subscribingRegistry{}, subscribeTestKM(), "no subscribe result"},
		{"keyset error", &subscribingRegistry{}, &mockKM{err: errors.New("vault sealed")}, "vault sealed"},
		{"no signing key", &subscribingRegistry{}, &mockKM{keyset: &model.Keyset{UniqueKeyID: "k"}}, "no signing public key"},
	}
	for _, tt := range tests {
		t.Run(tt.name+" is logged when not required", func(t *testing.T) {
			if err := subscribeToRegistry(context.Background(), tt.reg, tt.km, subscribeTestConfig()); err != nil {
				t.Fatalf("subscribeToRegistry() error = %v, want startup to continue", err)
			}
		})
		t.Run(tt.name+" fails startup when required", func(t *testing.T) {
			cfg := subscribeTestConfig()
			cfg.Subscribe.Required = true
			err := subscribeToRegistry(context.Background(), tt.reg, tt.km, cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("subscribeToRegistry() error = %v, want it to contain %q", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "failed to subscribe bap.example.com") {
				t.Errorf("error %q should name the subscriber", err)
			}
		})
	}
}

func TestSubscribeToRegistry_ReportsUnusableRecord(t *testing.T) {
	// A record ID other than the key ID, or a record that is not live, is
	// reported but does not fail startup even when subscribe is required.
	for _, result := range []*model.SubscribeResult{
		{Status: model.SubscribeCreated, RecordID: "other-id", State: "live"},
		{Status: model.SubscribeUnchanged, RecordID: "rec-id-1", State: "draft"},
		{Status: model.SubscribeUpdated},
	} {
		cfg := subscribeTestConfig()
		cfg.Subscribe.Required = true
		if err := subscribeToRegistry(context.Background(), &subscribingRegistry{result: result}, subscribeTestKM(), cfg); err != nil {
			t.Errorf("subscribeToRegistry(%+v) error = %v", result, err)
		}
	}
}

// subscribeMgr serves a subscribing registry and a key manager to NewStdHandler.
type subscribeMgr struct {
	noopPluginManager
	registry *subscribingRegistry
	km       definition.KeyManager
}

func (m *subscribeMgr) Registry(context.Context, definition.Cache, *plugin.Config) (definition.RegistryLookup, error) {
	return m.registry, nil
}

func (m *subscribeMgr) KeyManager(context.Context, definition.RegistryLookup, *plugin.Config) (definition.KeyManager, error) {
	return m.km, nil
}

func TestNewStdHandler_Subscribe(t *testing.T) {
	newCfg := func() *Config {
		cfg := subscribeTestConfig()
		cfg.Plugins.KeyManager = &plugin.Config{ID: "simplekeymanager"}
		return cfg
	}

	t.Run("subscribes at startup when configured", func(t *testing.T) {
		mgr := &subscribeMgr{
			registry: &subscribingRegistry{result: &model.SubscribeResult{Status: model.SubscribeCreated, RecordID: "rec-id-1", State: "live"}},
			km:       subscribeTestKM(),
		}
		if _, err := NewStdHandler(context.Background(), mgr, newCfg(), "bapTxnReceiver"); err != nil {
			t.Fatalf("NewStdHandler() error = %v", err)
		}
		if len(mgr.registry.calls) != 1 {
			t.Fatalf("expected 1 Subscribe call, got %d", len(mgr.registry.calls))
		}
	})

	t.Run("does not subscribe without a subscribe block", func(t *testing.T) {
		mgr := &subscribeMgr{registry: &subscribingRegistry{}, km: subscribeTestKM()}
		cfg := newCfg()
		cfg.Subscribe = nil
		if _, err := NewStdHandler(context.Background(), mgr, cfg, "bapTxnReceiver"); err != nil {
			t.Fatalf("NewStdHandler() error = %v", err)
		}
		if len(mgr.registry.calls) != 0 {
			t.Fatalf("expected no Subscribe call, got %d", len(mgr.registry.calls))
		}
	})

	t.Run("required failure fails startup", func(t *testing.T) {
		mgr := &subscribeMgr{registry: &subscribingRegistry{err: errors.New("registry down")}, km: subscribeTestKM()}
		cfg := newCfg()
		cfg.Subscribe.Required = true
		if _, err := NewStdHandler(context.Background(), mgr, cfg, "bapTxnReceiver"); err == nil || !strings.Contains(err.Error(), "registry down") {
			t.Fatalf("NewStdHandler() error = %v, want the subscribe failure", err)
		}
	})
}
