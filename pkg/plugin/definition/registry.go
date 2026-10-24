package definition

import (
	"context"
	"errors"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// ErrSubscribeNotConfigured is returned (wrapped) by a RegistrySubscriber whose own config
// does not say where to write the record, so callers can treat it as a config error.
var ErrSubscribeNotConfigured = errors.New("registry plugin is not configured to subscribe")

// RegistryLookup resolves Beckn subscriber identities using generic Beckn protocol parameters.
// Inputs are subscriber-ID and key-ID as carried in the Beckn Authorization header.
type RegistryLookup interface {
	// Lookup finds a registry entry by subscriberID and keyID (from the Authorization header)
	// and returns the subscriber's public keys for incoming message signature validation.
	// Input: req.SubscriberID (Beckn subscriber_id), req.KeyID (Beckn key_id).
	Lookup(ctx context.Context, req *model.Subscription) ([]model.Subscription, error)
}

// RegistryMetadataLookup resolves DeDi registry and node records using DeDi-native path parameters.
// All inputs use the DeDi namespace/registry(/recordName) path convention — these are not generic
// Beckn params and are not interchangeable with the subscriberID/keyID used by RegistryLookup.
type RegistryMetadataLookup interface {
	// LookupRegistry fetches registry-level metadata for a DeDi network registry.
	// Input: namespaceIdentifier (DeDi namespace, e.g. "nfh.global"),
	//        registryName (DeDi registry name, e.g. "retail.network.production").
	// Returns registry metadata including manifest URLs used by ManifestLoader.
	LookupRegistry(ctx context.Context, namespaceIdentifier, registryName string) (*model.RegistryMetadata, error)

	// LookupNode fetches the full subscriber record for a DeDi node by its NodeID.
	// Input: nodeID must be a fully-qualified three-part DeDi path in
	//        namespace/registry/recordName format (e.g. "nfh.global/subscribers.beckn.one/bpp.energy.com").
	// Returns a SubscriberRecord with subscriber identity/endpoint data and any node manifest
	// metadata from the same DeDi response. Meta is empty (not an error) when the participant
	// has not yet published a node manifest.
	// The full SubscriberRecord is available for any plugin to consume — manifest discovery
	// (ManifestLoader) is the first use case but not the only one.
	LookupNode(ctx context.Context, nodeID string) (*model.SubscriberRecord, error)

	// QueryByNetwork fetches all subscriber records belonging to a DeDi network registry.
	// Input: networkID in namespace/registryName DeDi path form (e.g. "beckn.one/testnet").
	// Only records with state=="live" are returned. Meta/MetaArrays follow the same shape
	// as LookupNode (e.g. MetaArrays["catalog_index_urls"]). AllowedNetworkIDs is not
	// applied, same as LookupNode -- this is a discovery read, not a trust decision.
	QueryByNetwork(ctx context.Context, networkID string) ([]model.SubscriberRecord, error)
}

// RegistrySubscriber is an optional capability of a RegistryLookup (found by type assertion)
// that publishes this adapter's own subscriber record. It must be idempotent: callers run it
// on every startup.
type RegistrySubscriber interface {
	// Subscribe creates, updates or leaves alone the configured record.
	// Input: req.SubscriberID, req.URL (callback), req.Type, req.SigningPublicKey and,
	// when set, req.EncrPublicKey and req.Domain.
	Subscribe(ctx context.Context, req *model.Subscription) (*model.SubscribeResult, error)
}

// RegistryLookupProvider initializes a new registry lookup instance.
type RegistryLookupProvider interface {
	New(context.Context, Cache, map[string]string) (RegistryLookup, func() error, error)
}
