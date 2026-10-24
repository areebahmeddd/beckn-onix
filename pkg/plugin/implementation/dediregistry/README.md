# DeDi Registry Plugin

A **registry type plugin** for Beckn-ONIX that integrates with DeDi (Decentralized Digital Infrastructure) registry services via the new DeDi Wrapper API.

## Overview

The DeDi Registry plugin implements the `RegistryLookup` interface to retrieve public keys and participant information from DeDi registry services. It's used by the KeyManager for **signature validation of incoming requests** from other network participants.

It can also publish the adapter's own subscriber record to DeDi (`definition.RegistrySubscriber`). This is opt-in and off by default. See [Subscribing](#subscribing-publishing-this-adapters-record).





## Configuration

```yaml
registry:
  id: dediregistry
  config:
    url: "https://fabric.nfh.global/registry/dedi"
    allowedNetworkIDs: "commerce-network.org/prod,local-commerce.org/production"
    timeout: 30
    retry_max: 3
    retry_wait_min: 1s
    retry_wait_max: 5s
```

### Configuration Parameters

| Parameter | Required | Description | Default |
|-----------|----------|-------------|---------|
| `url` | Yes | DeDi wrapper API base URL (include /dedi path). Must be an absolute `http(s)` URL with a host and no query or fragment (checked at startup); a trailing slash is trimmed | - |
| `allowedNetworkIDs` | No | Allowlist of network membership IDs from `data.network_memberships` for signature validation | - |
| `cacheTTL` | No | How long a `Lookup` result is cached: a Go duration with a unit (e.g. `300s`, `5m`); a bare number is rejected with a warning and the default is used, and zero or negative also means the default. A positive `data.ttl` in the DeDi response (in seconds) overrides it | 5m |
| `timeout` | No | Request timeout in seconds | Client default |
| `retry_max` | No | Maximum number of retry attempts | 4 (library default) |
| `retry_wait_min` | No | Minimum wait time between retries (e.g., "1s", "500ms") | 1s (library default) |
| `retry_wait_max` | No | Maximum wait time between retries (e.g., "5s") | 30s (library default) |
| `subscribeURL` | No | DeDi API base URL, including the `/dedi` path, that owns your record (e.g. `https://api.dedi.global/dedi`). This is not the read-only `url` cache. Setting it enables `Subscribe`, and the keys below are then required. Same URL checks as `url` | - (subscribe off) |
| `subscribeNamespace` | With `subscribeURL` | Your verified DeDi namespace (domain) | - |
| `subscribeRegistryName` | With `subscribeURL` | The registry in that namespace that uses the [Beckn Subscriber schema](https://raw.githubusercontent.com/LF-Decentralized-Trust-labs/decentralized-directory-protocol/refs/heads/main/schemas/beckn_subscriber.json) | - |
| `subscribeRecordName` | With `subscribeURL` | Record name, unique in the registry. Use one record per role | - |
| `subscribeCountries` | With `subscribeURL` | Comma-separated ISO 3166-1 alpha-3 codes for `details.countries` (the schema requires at least one) | - |
| `subscribeDescription` | No | Record description | Existing record: keeps its description. New record: `Beckn subscriber record for <subscriber_id> (<type>)` |

With `subscribeURL` set, the plugin reads the DeDi API key from the **`DEDI_API_KEY`** environment variable. It is not a config key, so the secret stays out of YAML, as with `REDIS_PASSWORD`. If the variable is unset, the plugin fails to load. Setting any other `subscribe*` key without `subscribeURL` is also a load error.

## API Integration

### Beckn Registry API Format
```
GET {url}/lookup/{subscriber_id}/subscribers.beckn.one/{key_id}
```

**Example**: `https://fabric.nfh.global/registry/dedi/lookup/bpp.example.com/subscribers.beckn.one/76EU7ofwRCF1aobQkShARrf1PAUsNpHqWUJoynPu9w45YFKmzqaPmy`

### Authentication
**No authentication required** - Beckn Registry API is public.

### Expected Response Format

```json
{
  "message": "Record retrieved from registry cache",
  "data": {
    "record_id": "76EU7ofwRCF1aobQkShARrf1PAUsNpHqWUJoynPu9w45YFKmzqaPmy",
    "details": {
      "url": "http://bpp.example.com/beckn/bap",
      "type": "BAP",
      "domain": "energy",
      "subscriber_id": "bpp.example.com",
      "signing_public_key": "384qqkIIpxo71WaJPsWqQNWUDGAFnfnJPxuDmtuBiLo=",
      "encr_public_key": "test-encr-key"
    },
    "network_memberships": ["commerce-network.org/prod", "local-commerce.org/production"],
    "created_at": "2025-10-27T11:45:27.963Z",
    "updated_at": "2025-10-27T11:46:23.563Z"
  }
}
```

## Network-Scoped Discovery (`QueryByNetwork`)

In addition to the single-record `RegistryLookup`/`RegistryMetadataLookup.LookupNode` calls above, the plugin implements `RegistryMetadataLookup.QueryByNetwork(ctx, networkID)`, which fetches every subscriber record belonging to a DeDi network registry in one call — used by `catalogcrawler` to discover the providers of each configured network, instead of a direct HTTP call to DeDi.

```
GET {url}/query/{networkID}
```

`networkID` is sent in `namespace/registryName` form (e.g. `beckn.one/testnet`), matching the DeDi path convention used elsewhere (`LookupRegistry`, `LookupNode`). Each `/`-separated segment is path-escaped, and an empty, whitespace-only, `.` or `..` segment is rejected with `CTX_INVALID_FIELD`.

### Expected Response Format

```json
{
  "data": {
    "records": [
      {
        "state": "live",
        "details": { "subscriber_id": "bpp.example.com", "url": "...", "type": "BPP", "domain": "energy" },
        "meta": { "catalog_index_urls": [{ "url": "https://bpp.example.com/catalog/index.json" }] }
      }
    ]
  }
}
```

Only records with `state == "live"` are returned; a record whose `details` don't parse is skipped rather than failing the whole query. `allowedNetworkIDs` is **not** applied to this call — like `LookupNode`, this is a discovery read, not a trust decision. Results are not cached (unlike `Lookup`, which is on the hot request-signing path).

## Subscribing (publishing this adapter's record)

`Subscribe(ctx, *model.Subscription)` implements `definition.RegistrySubscriber`. It writes the adapter's own Beckn Subscriber record (`subscriber_id`, `url`, `type`, `signing_public_key`, `countries`, plus `encr_public_key`/`domain` when set) into `{subscribeNamespace}/{subscribeRegistryName}/{subscribeRecordName}`. The adapter calls it at startup when a std handler has a [`subscribe` block](../../../../CONFIG.md#subscribe). The subscriber ID and keys come from that module's `subscriberId` and key manager.

### Flow (idempotent, safe on every restart)

```
1.  GET {subscribeURL}/lookup/{namespace}/{registry}/{record}             (public, no API key)
2a. 404 -> POST {subscribeURL}/{namespace}/{registry}/save-record-as-draft?publish=true
           {"record_name", "description", "details"}                       -> status "created"
2b. 200, details differ -> POST {subscribeURL}/{namespace}/{registry}/{record}/update-record
           {"description", "details", "meta"?}                             -> status "updated"
2c. 200, details match  -> no write                                         -> status "unchanged"
3.  GET the record again to report its record_id and state (best effort)
```

Writes send `Authorization: Bearer $DEDI_API_KEY` and `Content-Type: application/json`, and accept any 2xx. An update starts from the existing record. Details keys this adapter does not manage, the record's `meta` (e.g. `catalog_index_urls`), and its description (unless `subscribeDescription` is set) are sent back unchanged.

The result is a `model.SubscribeResult{Status, RecordID, State}`. The **record ID is the key ID** other participants look your signing key up by (`/lookup/{subscriber_id}/subscribers.beckn.one/{record_id}` above). The handler therefore warns when it differs from the key manager's `keyId`, or when the record is not `live`. On a first subscribe, DeDi assigns the record ID, so set your key manager's `keyId` to the value it reports.

### Where the API comes from

- **Lookup**: `GET /dedi/lookup/{namespace}/{registry_name}/{record_name}` is documented, and public (`security: []`), in the DeDi protocol's [OpenAPI](https://github.com/LF-Decentralized-Trust-labs/decentralized-directory-protocol/blob/main/api/openapi.yaml) (v2.0.0, servers `https://api.dedi.global` and `https://staging-api.dedi.global`). Its Bearer API-key scheme is the one used for writes.
- **Create / update**: these are **not in the published OpenAPI**, which only has lookup, query and versions. They are the calls the dedi.global publish portal (`https://publish.dedi.global`) makes for "Publish record" and "Update record". The portal is what the [NFH hosted onboarding guide](https://docs.nfh.global/build/onboarding/hosted.md) tells participants to use. API keys are generated in the portal. Without a key, `api.dedi.global` answers these routes `402 Payment Required`, while an unknown route returns `404 Cannot POST`.
- The other onboarding path, [file-based](https://docs.nfh.global/build/onboarding/file-based.md), publishes signed DeDi files (`/.well-known/dedi.index.json`) on the participant's own domain. It has no write API, so this plugin does not cover it.
- No Beckn request signature is involved: DeDi authenticates writes with the API key. The record carries your signing **public** key, which the adapter reads from the key manager.

**Open questions for maintainers / the DeDi team:** the write endpoints are undocumented and could change. The following are also unverified against a live record:
- whether `update-record` keeps a live record live;
- what a create returns for a record name that already exists only as a draft;
- whether the fabric lookup cache (`url`) can find a record by name rather than record ID.

### Subscribe errors

`Subscribe` runs at startup, not on a request path. Its errors therefore keep DeDi's response text, such as schema validation details. Other failures are classified like the lookups:

| Failure | Code | HTTP |
|---------|------|------|
| `subscribeURL` not configured | `NET_INTERNAL_ERROR` | 500 |
| Missing `subscriber_id`, `url`, `type` or `signing_public_key` | `CTX_MISSING_FIELD` | 400 |
| DeDi rejected the API key (401, 402, 403). The message names `DEDI_API_KEY` and the namespace | `NET_INTERNAL_ERROR` | 500 |
| DeDi rejected the record (other 4xx, e.g. a schema violation) | `NET_INTERNAL_ERROR` | 500 |
| Namespace or registry not found on write (404) | `NET_ENTITY_NOT_FOUND` † | 404 |
| Lookup or write: timeout / unreachable / 5xx or 429 after retries | `NET_TIMEOUT` / `NET_DOWNSTREAM_UNAVAILABLE` | 504 / 503 |
| Lookup returned an unusable 200 body | `NET_DOWNSTREAM_INVALID_RESPONSE` † | 502 |

A failed read-back after a successful write is logged and reported as an empty `RecordID`/`State`, not as an error. Writes are retried like lookups. A create retried after a 5xx that DeDi had in fact applied can therefore fail as a duplicate; the next startup then finds the record and updates it. Each call runs in a `registry subscribe` span carrying the subscriber ID, outcome status and record ID.

## Usage Context

### Signature Validation Flow
```
1. External ONIX → Request with Authorization header
2. ONIX Receiver → parseHeader() extracts subscriberID/keyID  
3. validateSign step → KeyManager.LookupNPKeys()
4. KeyManager → DeDiRegistry.Lookup() with extracted values
5. DeDi Registry → GET {url}/lookup/{subscriberID}/subscribers.beckn.one/{keyID}
6. DeDi Wrapper → Returns participant public keys
7. SignValidator → Validates signature using retrieved public key
```

### Module Configuration Example

```yaml
modules:
  - name: bppTxnReceiver
    handler:
      plugins:
        registry:
          id: dediregistry
          config:
            url: "https://fabric.nfh.global/registry/dedi"
            allowedNetworkIDs: "commerce-network.org/prod,local-commerce.org/production"
            timeout: 30
            retry_max: 3
            retry_wait_min: 1s
            retry_wait_max: 5s
      steps:
        - validateSign  # Required for registry lookup
        - addRoute
```

## Field Mapping

| DeDi Wrapper Field | Beckn Field | Description |
|-------------------|-------------|-------------|
| `data.details.subscriber_id` | `subscriber_id` | Participant identifier (empty if DeDi omits it) |
| `{key_id from URL}` | `key_id` | Unique key identifier |
| `data.details.signing_public_key` | `signing_public_key` | Public key for signature verification |
| `data.details.encr_public_key` | `encr_public_key` | Public key for encryption |
| `data.is_revoked` | `status` | Not mapped (Status field will be empty) |
| `data.created_at` | `created` | Creation timestamp |
| `data.updated_at` | `updated` | Last update timestamp |

## Features

- **No Authentication Required for lookups**: DeDi wrapper API doesn't require API keys (only the opt-in `Subscribe` writes use one)
- **GET Request Format**: Simple URL-based parameter passing
- **Classified Errors**: Every request-time failure maps to an error code and NACK status by cause (see [Error Handling](#error-handling))
- **Simplified Response**: Focuses on public key retrieval for signature validation
- **Retry Support**: Built-in retry mechanism for network resilience

## Testing

Run the test suite:

```bash
go test ./pkg/plugin/implementation/dediregistry -v
```

The tests cover:
- URL construction validation
- Response parsing for new API format
- Error-code classification of every failure path (`dediregistry_errors_test.go` and the error cases in `dediregistry_test.go`)
- Configuration validation
- Plugin provider functionality
- `Subscribe` against an in-memory DeDi (`subscribe_test.go`): create, update (keeping `meta` and unmanaged details), unchanged, retries, auth/schema/5xx/malformed failures, path escaping, and API-key redaction

## Migration Notes

This plugin replaces direct DeDi API integration with the new DeDi Wrapper API format:

- **Removed**: API key authentication, namespaceID parameters
- **Changed**: POST requests → GET requests
- **Updated**: Response structure parsing (`data.details` object)
- **Updated**: Optional allowlist validation now checks `data.network_memberships`
- **Deprecated**: `allowedParentNamespaces` config key in favor of `allowedNetworkIDs` (plugin now errors until the config is updated to full network membership IDs)
- **Added**: New URL path parameter format

## Dependencies

- `github.com/hashicorp/go-retryablehttp`: HTTP client with retry logic
- Standard Go libraries for HTTP and JSON handling

## Error Handling

A `url` that fails the startup check above stops the plugin from loading. Every failure at request time is returned as a `*model.CodedErr` carrying an error code and an HTTP status.

The HTTP status becomes the NACK's only where a request handler propagates the error with `%w`. Today that is `Lookup`, via `keymanager` / `simplekeymanager` and core's signature validation. Other callers handle the errors themselves:
- `catalogcrawler` and `catalogpublisher` signature verification (`Lookup`, via `internal/registrykey`) treat any lookup error as a transient crawl or verify fault.
- `schemaversionmediator`'s per-request counterparty `LookupNode` follows its `onFailure` setting (`reject` → `SCH_SCHEMA_ADAPTATION_FAILED`). A failed startup lookup of its own `nodeId` marks the node not onboarded (`SCH_SUBSCRIBER_NOT_FOUND`).
- `catalogpublisher`'s optional self-lookup (`LookupNode`) only logs a failure at Warn.
- `LookupRegistry` runs only at policy load, and `QueryByNetwork` only in the catalog crawler.

Most codes are Beckn v2.0.0 `ErrorCode` values. Those marked † are ONIX codes, used where no spec value names the cause.

| Failure | Code | HTTP |
|---------|------|------|
| DeDi request timed out (client `timeout` or caller context deadline) | `NET_TIMEOUT` | 504 |
| DeDi unreachable (DNS, dial, TLS, connection reset, redirect loop, response body cut short) | `NET_DOWNSTREAM_UNAVAILABLE` | 503 |
| DeDi returned 5xx or 429 after retries were exhausted, or any other non-200 status outside 4xx (e.g. 501, 202) | `NET_DOWNSTREAM_UNAVAILABLE` | 503 |
| DeDi returned 200 with an unusable body: not JSON; for `Lookup`/`LookupNode`, missing or non-object `data` or `details`; for `LookupRegistry`, missing or non-object `data` or `meta`; for `QueryByNetwork`, not the `{"data":{"records":[...]}}` shape (a body with no `data` is an empty result, not an error) | `NET_DOWNSTREAM_INVALID_RESPONSE` † | 502 |
| DeDi response body over the read cap (1 MiB for lookups, 8 MiB for `/query`) | `NET_DOWNSTREAM_INVALID_RESPONSE` † | 502 |
| Caller's request was cancelled mid-lookup | `NET_REQUEST_CANCELLED` † | 500 |
| DeDi returned any other 4xx (the adapter sent a request DeDi rejected) | `NET_INTERNAL_ERROR` | 500 |
| Request could not be built (adapter-side) | `NET_INTERNAL_ERROR` | 500 |
| `Lookup`: empty, whitespace-only, `.` or `..` subscriber ID or key ID (malformed signature `keyId`) | `AUT_SIGNATURE_INVALID` | 401 |
| `Lookup`: DeDi returned 404, or a record whose `subscriber_id` is a different subscriber (compared case-insensitively; a record with no `subscriber_id` is accepted, and its empty `subscriber_id` is returned as is) | `AUT_SUBSCRIBER_NOT_FOUND` | 401 |
| `Lookup`: record has no `signing_public_key` | `AUT_KEY_NOT_FOUND` | 401 |
| `Lookup`: subscriber not in `allowedNetworkIDs`, or `context.network_id` not in its memberships / `allowedNetworkIDs` | `AUT_NETWORK_NOT_ALLOWED` † | 401 |
| `LookupNode` / `LookupRegistry` / `QueryByNetwork`: DeDi returned 404 | `NET_ENTITY_NOT_FOUND` † | 404 |
| `LookupNode` / `LookupRegistry` / `QueryByNetwork`: empty node ID, namespace, registry name or network ID | `CTX_MISSING_FIELD` | 400 |
| `LookupNode` / `LookupRegistry` / `QueryByNetwork`: `nodeID` not in `namespace/registry/recordName` form, or a path argument with an empty, whitespace-only, `.` or `..` segment | `CTX_INVALID_FIELD` | 400 |

Transient failures before a response arrives (dial, DNS, connection reset, client timeout), 5xx (except 501) and 429 are retried per `retry_max` before being classified. A response body cut short while being read, redirect loops, TLS certificate failures, and a cancelled or expired caller context are not retried. `QueryByNetwork` skips individual records that don't parse, rather than failing.

Transport, request-build, oversized-response and `allowedNetworkIDs` failures report a generic message in the NACK that names no DeDi URL, host or adapter config. Registry and adapter faults are logged at Error level with their detailed cause, including a record returned for a different subscriber. Routine, caller-triggered outcomes are not logged as errors.
