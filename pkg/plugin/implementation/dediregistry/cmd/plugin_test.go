package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/dediregistry"
)

func TestDediRegistryProvider_ParseConfig(t *testing.T) {
	provider := dediRegistryProvider{}

	cfg, err := provider.parseConfig(map[string]string{
		"url":            "https://test.com/dedi",
		"timeout":        "30",
		"retry_max":      "5",
		"retry_wait_min": "100ms",
		"retry_wait_max": "2s",
	})
	if err != nil {
		t.Fatalf("parseConfig() error = %v", err)
	}

	if cfg.URL != "https://test.com/dedi" {
		t.Fatalf("expected URL to be parsed, got %q", cfg.URL)
	}
	if cfg.Timeout != 30 {
		t.Fatalf("expected Timeout 30, got %d", cfg.Timeout)
	}
	if cfg.RetryMax != 5 {
		t.Fatalf("expected RetryMax 5, got %d", cfg.RetryMax)
	}
	if cfg.RetryWaitMin != 100*time.Millisecond {
		t.Fatalf("expected RetryWaitMin 100ms, got %v", cfg.RetryWaitMin)
	}
	if cfg.RetryWaitMax != 2*time.Second {
		t.Fatalf("expected RetryWaitMax 2s, got %v", cfg.RetryWaitMax)
	}
}

func TestDediRegistryProvider_ParseConfig_InvalidConfig(t *testing.T) {
	provider := dediRegistryProvider{}

	tests := []struct {
		name        string
		config      map[string]string
		expectedErr string
	}{
		{
			name: "invalid timeout",
			config: map[string]string{
				"url":     "https://test.com/dedi",
				"timeout": "abc",
			},
			expectedErr: "invalid timeout value 'abc'",
		},
		{
			name: "invalid retry_max",
			config: map[string]string{
				"url":       "https://test.com/dedi",
				"retry_max": "abc",
			},
			expectedErr: "invalid retry_max value 'abc'",
		},
		{
			name: "negative retry_max",
			config: map[string]string{
				"url":       "https://test.com/dedi",
				"retry_max": "-1",
			},
			expectedErr: "retry_max must be non-negative",
		},
		{
			name: "invalid retry_wait_min",
			config: map[string]string{
				"url":            "https://test.com/dedi",
				"retry_wait_min": "notaduration",
			},
			expectedErr: "invalid retry_wait_min value 'notaduration'",
		},
		{
			name: "negative retry_wait_min",
			config: map[string]string{
				"url":            "https://test.com/dedi",
				"retry_wait_min": "-100ms",
			},
			expectedErr: "retry_wait_min must be non-negative",
		},
		{
			name: "invalid retry_wait_max",
			config: map[string]string{
				"url":            "https://test.com/dedi",
				"retry_wait_max": "notaduration",
			},
			expectedErr: "invalid retry_wait_max value 'notaduration'",
		},
		{
			name: "negative retry_wait_max",
			config: map[string]string{
				"url":            "https://test.com/dedi",
				"retry_wait_max": "-2s",
			},
			expectedErr: "retry_wait_max must be non-negative",
		},
		{
			name: "zero timeout",
			config: map[string]string{
				"url":     "https://test.com/dedi",
				"timeout": "0",
			},
			expectedErr: "timeout must be positive",
		},
		{
			name: "negative timeout",
			config: map[string]string{
				"url":     "https://test.com/dedi",
				"timeout": "-5",
			},
			expectedErr: "timeout must be positive",
		},
		{
			name: "retry_wait_min exceeds retry_wait_max",
			config: map[string]string{
				"url":            "https://test.com/dedi",
				"retry_wait_min": "5s",
				"retry_wait_max": "1s",
			},
			expectedErr: "retry_wait_min (5s) must not exceed retry_wait_max (1s)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := provider.parseConfig(tt.config)
			if err == nil {
				t.Fatal("expected parseConfig() to return an error")
			}
			if cfg != nil {
				t.Fatalf("expected nil config on error, got %#v", cfg)
			}
			if !strings.Contains(err.Error(), tt.expectedErr) {
				t.Fatalf("expected error to contain %q, got %q", tt.expectedErr, err.Error())
			}
		})
	}
}

// TestDediRegistryProvider_ParseConfig_CacheTTL verifies that cacheTTL is parsed correctly
// and that an invalid value warns but does not return an error (warn-and-ignore semantics).
func TestDediRegistryProvider_ParseConfig_CacheTTL(t *testing.T) {
	provider := dediRegistryProvider{}

	t.Run("valid cacheTTL is parsed and set", func(t *testing.T) {
		cfg, err := provider.parseConfig(map[string]string{
			"url":      "https://test.com/dedi",
			"cacheTTL": "15m",
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if cfg.CacheTTL != 15*time.Minute {
			t.Errorf("expected CacheTTL 15m, got %v", cfg.CacheTTL)
		}
	})

	t.Run("invalid cacheTTL warns and leaves CacheTTL zero", func(t *testing.T) {
		cfg, err := provider.parseConfig(map[string]string{
			"url":      "https://test.com/dedi",
			"cacheTTL": "not-a-duration",
		})
		if err != nil {
			t.Fatalf("expected no error (warn-and-ignore), got %v", err)
		}
		if cfg.CacheTTL != 0 {
			t.Errorf("expected CacheTTL 0 (will use defaultCacheTTL), got %v", cfg.CacheTTL)
		}
	})
}

func TestDediRegistryProvider_New_InvalidRetryConfig(t *testing.T) {
	provider := dediRegistryProvider{}

	_, _, err := provider.New(context.Background(), nil, map[string]string{
		"url":       "https://test.com/dedi",
		"retry_max": "abc",
	})
	if err == nil {
		t.Fatal("expected New() to return an error for invalid retry config")
	}
	if !strings.Contains(err.Error(), "failed to parse DeDi registry configuration") {
		t.Fatalf("expected wrapped parse error, got %q", err.Error())
	}
}

func TestDediRegistryProvider_New_ForwardsRetryConfig(t *testing.T) {
	var captured *dediregistry.Config
	provider := dediRegistryProvider{
		newFunc: func(ctx context.Context, cache definition.Cache, cfg *dediregistry.Config) (*dediregistry.DeDiRegistryClient, func() error, error) {
			captured = cfg
			return new(dediregistry.DeDiRegistryClient), func() error { return nil }, nil
		},
	}

	config := map[string]string{
		"url":            "https://test.com/dedi",
		"timeout":        "30",
		"retry_max":      "5",
		"retry_wait_min": "100ms",
		"retry_wait_max": "2s",
	}

	_, closer, err := provider.New(context.Background(), nil, config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if closer == nil {
		t.Fatal("expected closer to be non-nil")
	}
	if err := closer(); err != nil {
		t.Fatalf("closer() error = %v", err)
	}
	if captured == nil {
		t.Fatal("expected config to be forwarded to client constructor")
	}
	if captured.RetryMax != 5 {
		t.Fatalf("expected RetryMax 5, got %d", captured.RetryMax)
	}
	if captured.RetryWaitMin != 100*time.Millisecond {
		t.Fatalf("expected RetryWaitMin 100ms, got %v", captured.RetryWaitMin)
	}
	if captured.RetryWaitMax != 2*time.Second {
		t.Fatalf("expected RetryWaitMax 2s, got %v", captured.RetryWaitMax)
	}
}

func TestDediRegistryProvider_New(t *testing.T) {
	ctx := context.Background()
	provider := dediRegistryProvider{newFunc: dediregistry.New}

	config := map[string]string{
		"url":     "https://test.com/dedi",
		"timeout": "30",
	}

	dediRegistry, closer, err := provider.New(ctx, nil, config)
	if err != nil {
		t.Errorf("New() error = %v", err)
		return
	}

	if dediRegistry == nil {
		t.Error("New() returned nil dediRegistry")
	}

	if closer == nil {
		t.Error("New() returned nil closer")
	}

	// Test cleanup
	if err := closer(); err != nil {
		t.Errorf("closer() error = %v", err)
	}
}

func TestDediRegistryProvider_New_InvalidConfig(t *testing.T) {
	ctx := context.Background()
	provider := dediRegistryProvider{newFunc: dediregistry.New}

	tests := []struct {
		name   string
		config map[string]string
	}{
		{
			name:   "missing url",
			config: map[string]string{"timeout": "30"},
		},
		{
			name:   "empty config",
			config: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := provider.New(ctx, nil, tt.config)
			if err == nil {
				t.Errorf("New() with %s should return error", tt.name)
			}
		})
	}
}

func TestDediRegistryProvider_New_InvalidTimeout(t *testing.T) {
	// Invalid timeout is now a hard error, not a warn-and-continue fallback.
	provider := dediRegistryProvider{newFunc: dediregistry.New}

	_, _, err := provider.New(context.Background(), nil, map[string]string{
		"url":     "https://test.com/dedi",
		"timeout": "invalid",
	})
	if err == nil {
		t.Fatal("expected New() to return an error for invalid timeout")
	}
	if !strings.Contains(err.Error(), "invalid timeout value 'invalid'") {
		t.Fatalf("expected timeout parse error, got %q", err.Error())
	}
}

func TestParseAllowedNetworkIDs(t *testing.T) {
	got := parseAllowedNetworkIDs("commerce-network.org/prod, local-commerce.org/production, ,")
	want := []string{
		"commerce-network.org/prod",
		"local-commerce.org/production",
	}

	if len(got) != len(want) {
		t.Fatalf("expected %d allowed network IDs, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("expected allowedNetworkIDs[%d] to preserve input order as %q, got %q", i, want[i], got[i])
		}
	}
}

func TestResolveAllowedNetworkIDs_DeprecatedAllowedParentNamespacesErrorsWithoutAllowedNetworkIDs(t *testing.T) {
	config := map[string]string{
		"allowedParentNamespaces": "commerce-network.org/prod, local-commerce.org/production",
	}

	got, err := resolveAllowedNetworkIDs(config)
	if err == nil {
		t.Fatal("expected error when only allowedParentNamespaces is configured")
	}
	if got != nil {
		t.Fatalf("expected nil allowed network IDs on error, got %#v", got)
	}
}

func TestResolveAllowedNetworkIDs_AllowedNetworkIDsTakesPrecedence(t *testing.T) {
	config := map[string]string{
		"url":                     "https://test.com/dedi",
		"allowedParentNamespaces": "deprecated-network.org/legacy",
		"allowedNetworkIDs":       "commerce-network.org/prod, local-commerce.org/production",
	}

	got, err := resolveAllowedNetworkIDs(config)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	want := []string{
		"commerce-network.org/prod",
		"local-commerce.org/production",
	}

	if len(got) != len(want) {
		t.Fatalf("expected %d allowed network IDs, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("expected allowedNetworkIDs[%d] = %q, got %q", i, want[i], got[i])
		}
	}
}

func TestDediRegistryProvider_New_DeprecatedAllowedParentNamespacesErrorsWithoutAllowedNetworkIDs(t *testing.T) {
	ctx := context.Background()
	provider := dediRegistryProvider{}

	config := map[string]string{
		"url":                     "https://test.com/dedi",
		"allowedParentNamespaces": "commerce-network.org",
	}

	_, _, err := provider.New(ctx, nil, config)
	if err == nil {
		t.Fatal("expected New() to error when only allowedParentNamespaces is configured")
	}
}

func TestDediRegistryProvider_New_NilContext(t *testing.T) {
	provider := dediRegistryProvider{}

	config := map[string]string{
		"url": "https://test.com/dedi",
	}

	_, _, err := provider.New(nil, nil, config)
	if err == nil {
		t.Error("New() with nil context should return error")
	}
	if err.Error() != "context cannot be nil" {
		t.Errorf("Expected specific error message, got %v", err)
	}
}

func TestParseSubscribeConfig(t *testing.T) {
	t.Run("unset leaves the plugin lookup-only", func(t *testing.T) {
		cfg, err := dediRegistryProvider{}.parseConfig(map[string]string{"url": "https://test.com/dedi"})
		if err != nil {
			t.Fatalf("parseConfig() error = %v", err)
		}
		if cfg.Subscribe != nil {
			t.Fatalf("expected no subscribe config, got %+v", cfg.Subscribe)
		}
	})

	t.Run("subscribe keys without subscribeURL are rejected", func(t *testing.T) {
		_, err := dediRegistryProvider{}.parseConfig(map[string]string{
			"url":                "https://test.com/dedi",
			"subscribeNamespace": "np.example.com",
		})
		if err == nil || !strings.Contains(err.Error(), "subscribeNamespace is set but subscribeURL is empty") {
			t.Fatalf("parseConfig() error = %v", err)
		}
	})

	t.Run("full config", func(t *testing.T) {
		t.Setenv(dediregistry.APIKeyEnv, "secret-key")
		cfg, err := dediRegistryProvider{}.parseConfig(map[string]string{
			"url":                   "https://test.com/dedi",
			"subscribeURL":          "https://api.dedi.global/dedi",
			"subscribeNamespace":    " np.example.com ",
			"subscribeRegistryName": "nfo-x_mobility_prod_registry",
			"subscribeRecordName":   "bap-record",
			"subscribeCountries":    "IND, USA,",
			"subscribeDescription":  "NP BAP",
		})
		if err != nil {
			t.Fatalf("parseConfig() error = %v", err)
		}
		want := &dediregistry.SubscribeConfig{
			URL:          "https://api.dedi.global/dedi",
			Namespace:    "np.example.com",
			RegistryName: "nfo-x_mobility_prod_registry",
			RecordName:   "bap-record",
			Countries:    []string{"IND", "USA"},
			Description:  "NP BAP",
			APIKey:       "secret-key",
		}
		if !reflect.DeepEqual(cfg.Subscribe, want) {
			t.Fatalf("Subscribe = %+v, want %+v", cfg.Subscribe, want)
		}
	})
}

func TestDediRegistryProvider_New_Subscribe(t *testing.T) {
	config := map[string]string{
		"url":                   "https://test.com/dedi",
		"subscribeURL":          "https://api.dedi.global/dedi",
		"subscribeNamespace":    "np.example.com",
		"subscribeRegistryName": "registry",
		"subscribeRecordName":   "bap-record",
		"subscribeCountries":    "IND",
	}

	t.Run("missing API key fails the load", func(t *testing.T) {
		t.Setenv(dediregistry.APIKeyEnv, "")
		_, _, err := Provider.New(context.Background(), nil, config)
		if err == nil || !strings.Contains(err.Error(), dediregistry.APIKeyEnv) {
			t.Fatalf("New() error = %v, want the missing API key", err)
		}
	})

	t.Run("returns a RegistrySubscriber", func(t *testing.T) {
		t.Setenv(dediregistry.APIKeyEnv, "secret-key")
		registry, closer, err := Provider.New(context.Background(), nil, config)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		t.Cleanup(func() { _ = closer() })
		if _, ok := registry.(definition.RegistrySubscriber); !ok {
			t.Fatalf("%T does not implement definition.RegistrySubscriber", registry)
		}
	})
}
