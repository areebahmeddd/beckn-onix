package handler

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
)

// defaultSubscribeTimeout applies when subscribe.timeout is unset. Startup has
// no deadline and the registry client may have none, so a hung registry would
// otherwise keep the adapter from ever listening.
const defaultSubscribeTimeout = time.Minute

// subscribeToRegistry publishes the module's subscriber record when
// cfg.Subscribe is set. Configuration errors always fail startup; a subscribe
// failure fails it only when subscribe.required is set, so a registry outage
// cannot keep the adapter down.
func subscribeToRegistry(ctx context.Context, registry definition.RegistryLookup, km definition.KeyManager, cfg *Config) error {
	sc := cfg.Subscribe
	if sc == nil {
		return nil
	}
	subscriber, err := subscribeTarget(registry, km, cfg)
	if err != nil {
		return fmt.Errorf("invalid subscribe configuration: %w", err)
	}
	participantType, err := subscriberType(sc.Type, cfg.Role)
	if err != nil {
		return fmt.Errorf("invalid subscribe configuration: %w", err)
	}

	timeout := sc.Timeout
	if timeout <= 0 {
		timeout = defaultSubscribeTimeout
	}
	subCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, keyID, err := subscribe(subCtx, subscriber, km, cfg.SubscriberID, sc.URL, participantType)
	if err != nil {
		if errors.Is(err, definition.ErrSubscribeNotConfigured) {
			return fmt.Errorf("invalid subscribe configuration: %w", err)
		}
		if sc.Required {
			return fmt.Errorf("failed to subscribe %s to the registry: %w", cfg.SubscriberID, err)
		}
		log.Errorf(ctx, err, "Failed to subscribe %s to the registry; continuing startup because subscribe.required is false", cfg.SubscriberID)
		return nil
	}
	reportSubscribeResult(ctx, cfg.SubscriberID, keyID, result)
	return nil
}

// subscribeTarget validates what subscribing needs and returns the registry as
// a RegistrySubscriber.
func subscribeTarget(registry definition.RegistryLookup, km definition.KeyManager, cfg *Config) (definition.RegistrySubscriber, error) {
	if registry == nil {
		return nil, errors.New("subscribe requires a registry plugin")
	}
	subscriber, ok := registry.(definition.RegistrySubscriber)
	if !ok {
		return nil, fmt.Errorf("registry plugin %T does not support subscribe", registry)
	}
	if km == nil {
		return nil, errors.New("subscribe requires a keyManager plugin to read the public keys from")
	}
	if cfg.SubscriberID == "" {
		return nil, errors.New("subscribe requires subscriberId")
	}
	u, err := url.Parse(cfg.Subscribe.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("subscribe.url must be an absolute http(s) URL, got %q", cfg.Subscribe.URL)
	}
	return subscriber, nil
}

// subscriberType defaults the participant type to the role for bap and bpp.
func subscriberType(configured string, role model.Role) (string, error) {
	if configured != "" {
		return configured, nil
	}
	switch role {
	case model.RoleBAP, model.RoleBPP:
		return strings.ToUpper(string(role)), nil
	default:
		return "", fmt.Errorf("subscribe.type is required for role %q", role)
	}
}

// subscribe publishes the key manager's public keys for subscriberID and also
// returns the keyset's key ID.
func subscribe(ctx context.Context, subscriber definition.RegistrySubscriber, km definition.KeyManager, subscriberID, callbackURL, participantType string) (*model.SubscribeResult, string, error) {
	keyset, err := km.Keyset(ctx, subscriberID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read keyset for %s: %w", subscriberID, err)
	}
	if keyset.SigningPublic == "" {
		return nil, "", fmt.Errorf("keyset for %s has no signing public key", subscriberID)
	}
	result, err := subscriber.Subscribe(ctx, &model.Subscription{
		Subscriber: model.Subscriber{
			SubscriberID: subscriberID,
			URL:          callbackURL,
			Type:         participantType,
		},
		KeyID:            keyset.UniqueKeyID,
		SigningPublicKey: keyset.SigningPublic,
		EncrPublicKey:    keyset.EncrPublic,
	})
	if err != nil {
		return nil, "", err
	}
	if result == nil {
		return nil, "", errors.New("registry returned no subscribe result")
	}
	return result, keyset.UniqueKeyID, nil
}

// reportSubscribeResult warns when the record ID differs from the key ID (DeDi
// serves the signing key under the record ID, but peers look it up by the key
// ID in our Authorization header) or when the record is not live.
func reportSubscribeResult(ctx context.Context, subscriberID, keyID string, result *model.SubscribeResult) {
	log.Infof(ctx, "Subscribed %s to the registry: status=%s record_id=%q state=%q", subscriberID, result.Status, result.RecordID, result.State)
	if result.RecordID != "" && result.RecordID != keyID {
		log.Warnf(ctx, "Registry record ID %q for %s differs from the key manager's key ID %q; participants that look your signing key up by key ID will not find it until the key manager's keyId is set to %q",
			result.RecordID, subscriberID, keyID, result.RecordID)
	}
	if result.State != "" && result.State != "live" {
		log.Warnf(ctx, "Registry record for %s is in state %q, not \"live\"; other participants may not see it yet", subscriberID, result.State)
	}
}
