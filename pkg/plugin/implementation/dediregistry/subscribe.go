package dediregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/plugin/definition"
	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"github.com/hashicorp/go-retryablehttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The create and update endpoints are not in DeDi's published OpenAPI; they are
// the calls the publish portal (https://publish.dedi.global) makes. Their
// response bodies are never parsed: the record is read back through the
// documented lookup instead.
const (
	subscribeCreateURLFmt = "%s/%s/%s/save-record-as-draft?publish=true"
	subscribeUpdateURLFmt = "%s/%s/%s/%s/update-record"
	subscribeLookupURLFmt = "%s/lookup/%s/%s/%s"
)

// APIKeyEnv is the environment variable holding the DeDi API key for writes,
// kept out of YAML like REDIS_PASSWORD.
const APIKeyEnv = "DEDI_API_KEY"

// SubscribeConfig says where Subscribe writes the subscriber record.
type SubscribeConfig struct {
	// URL is the DeDi API base URL including /dedi, not the read-only lookup
	// cache in Config.URL.
	URL          string   `yaml:"url" json:"url"`
	Namespace    string   `yaml:"namespace" json:"namespace"`
	RegistryName string   `yaml:"registryName" json:"registryName"`
	RecordName   string   `yaml:"recordName" json:"recordName"`
	Countries    []string `yaml:"countries" json:"countries"`
	// Description, when empty, keeps an existing record's description and
	// generates one for a new record.
	Description string `yaml:"description" json:"description"`
	APIKey      string `yaml:"-" json:"-"`
}

// String omits the API key so the config is safe to log.
func (s SubscribeConfig) String() string {
	return fmt.Sprintf("{URL:%s Namespace:%s RegistryName:%s RecordName:%s Countries:%v Description:%q APIKeySet:%t}",
		s.URL, s.Namespace, s.RegistryName, s.RecordName, s.Countries, s.Description, s.APIKey != "")
}

// validateSubscribe fails the plugin load on a misconfiguration rather than
// the first Subscribe.
func validateSubscribe(s *SubscribeConfig) error {
	if err := validateBaseURL("subscribeURL", s.URL); err != nil {
		return err
	}
	if s.APIKey == "" {
		return fmt.Errorf("subscribe requires a DeDi API key in the %s environment variable", APIKeyEnv)
	}
	for _, f := range []struct{ key, value string }{
		{"subscribeNamespace", s.Namespace},
		{"subscribeRegistryName", s.RegistryName},
		{"subscribeRecordName", s.RecordName},
	} {
		if f.value == "" {
			return fmt.Errorf("%s cannot be empty", f.key)
		}
		if !validSegment(f.value) || strings.Contains(f.value, "/") {
			return fmt.Errorf("%s %q is not a valid DeDi path segment", f.key, f.value)
		}
	}
	if len(s.Countries) == 0 {
		return errors.New("subscribeCountries must list at least one ISO 3166-1 alpha-3 country code")
	}
	return nil
}

var _ definition.RegistrySubscriber = (*DeDiRegistryClient)(nil)

// errSubscriberRecordNotFound marks a lookup 404, which Subscribe answers by
// creating the record.
var errSubscriberRecordNotFound = errors.New("DeDi subscriber record not found")

func subscriberRecordNotFound(err error) *model.CodedErr {
	return entityNotFound(fmt.Errorf("%w: %w", errSubscriberRecordNotFound, err))
}

// Subscribe implements definition.RegistrySubscriber. An update sends back the
// fields req does not manage (other details keys, meta, the description) so it
// cannot drop e.g. meta.catalog_index_urls.
func (c *DeDiRegistryClient) Subscribe(ctx context.Context, req *model.Subscription) (result *model.SubscribeResult, err error) {
	tracer := otel.Tracer(telemetry.ScopeName, trace.WithInstrumentationVersion(telemetry.ScopeVersion))
	ctx, span := tracer.Start(ctx, "registry subscribe")
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "DeDi subscribe failed")
		} else {
			span.SetAttributes(
				attribute.String("registry.subscribe.status", string(result.Status)),
				attribute.String("registry.subscribe.record_id", result.RecordID),
			)
		}
		span.End()
	}()

	sc := c.config.Subscribe
	if sc == nil {
		return nil, internalErr(fmt.Errorf("%w: set subscribeURL and the other subscribe* keys in the dediregistry plugin config", definition.ErrSubscribeNotConfigured))
	}
	if req == nil {
		return nil, model.NewBadReqErr(codeCtxMissingField, errors.New("subscription is required for DeDi subscribe"))
	}
	for _, f := range []struct{ name, value string }{
		{"subscriber_id", req.SubscriberID},
		{"url", req.URL},
		{"type", req.Type},
		{"signing_public_key", req.SigningPublicKey},
	} {
		if strings.TrimSpace(f.value) == "" {
			return nil, model.NewBadReqErr(codeCtxMissingField, fmt.Errorf("%s is required for DeDi subscribe", f.name))
		}
	}
	span.SetAttributes(attribute.String("registry.subscribe.subscriber_id", req.SubscriberID))

	ns, reg, rec := url.PathEscape(sc.Namespace), url.PathEscape(sc.RegistryName), url.PathEscape(sc.RecordName)
	lookupURL := fmt.Sprintf(subscribeLookupURLFmt, sc.URL, ns, reg, rec)
	const lookupOp = "subscriber record lookup"

	var status model.SubscribeStatus
	existing, err := c.fetchDeDiData(ctx, lookupURL, lookupOp, subscriberRecordNotFound)
	switch {
	case errors.Is(err, errSubscriberRecordNotFound):
		status = model.SubscribeCreated
		body := map[string]any{
			"record_name": sc.RecordName,
			"description": subscribeDescription(sc.Description, "", req),
			"details":     subscriberDetails(sc, req),
		}
		err = c.doPost(ctx, fmt.Sprintf(subscribeCreateURLFmt, sc.URL, ns, reg), "subscriber record create", body)
	case err != nil:
		return nil, err
	default:
		body, changed := recordUpdate(sc, req, existing)
		if !changed {
			recordID, state := recordIdentity(existing)
			log.Infof(ctx, "DeDi subscribe: record %s/%s/%s already up to date", sc.Namespace, sc.RegistryName, sc.RecordName)
			return &model.SubscribeResult{Status: model.SubscribeUnchanged, RecordID: recordID, State: state}, nil
		}
		status = model.SubscribeUpdated
		err = c.doPost(ctx, fmt.Sprintf(subscribeUpdateURLFmt, sc.URL, ns, reg, rec), "subscriber record update", body)
	}
	if err != nil {
		return nil, err
	}

	result = &model.SubscribeResult{Status: status}
	if data, lerr := c.fetchDeDiData(ctx, lookupURL, lookupOp, subscriberRecordNotFound); lerr != nil {
		log.Warnf(ctx, "DeDi subscribe: record %s/%s/%s was %s, but reading it back failed: %v",
			sc.Namespace, sc.RegistryName, sc.RecordName, status, lerr)
	} else {
		result.RecordID, result.State = recordIdentity(data)
	}
	log.Infof(ctx, "DeDi subscribe: record %s/%s/%s %s (record_id=%q, state=%q)",
		sc.Namespace, sc.RegistryName, sc.RecordName, status, result.RecordID, result.State)
	return result, nil
}

// subscriberDetails builds the Beckn Subscriber details object. Values use the
// types encoding/json decodes into, so reflect.DeepEqual against a looked-up
// record works.
func subscriberDetails(sc *SubscribeConfig, req *model.Subscription) map[string]any {
	countries := make([]any, len(sc.Countries))
	for i, c := range sc.Countries {
		countries[i] = c
	}
	details := map[string]any{
		"subscriber_id":      req.SubscriberID,
		"url":                req.URL,
		"type":               req.Type,
		"signing_public_key": req.SigningPublicKey,
		"countries":          countries,
	}
	if req.EncrPublicKey != "" {
		details["encr_public_key"] = req.EncrPublicKey
	}
	if req.Domain != "" {
		details["domain"] = req.Domain
	}
	return details
}

// subscribeDescription falls back to a generated description because the
// portal requires one.
func subscribeDescription(configured, existing string, req *model.Subscription) string {
	switch {
	case configured != "":
		return configured
	case existing != "":
		return existing
	default:
		return fmt.Sprintf("Beckn subscriber record for %s (%s)", req.SubscriberID, req.Type)
	}
}

// recordUpdate builds the update-record body for an existing record and
// reports whether it differs from it.
func recordUpdate(sc *SubscribeConfig, req *model.Subscription, existing map[string]any) (map[string]any, bool) {
	existingDetails, _ := existing["details"].(map[string]any)
	details := maps.Clone(existingDetails)
	if details == nil {
		details = map[string]any{}
	}
	maps.Copy(details, subscriberDetails(sc, req))

	existingDescription, _ := existing["description"].(string)
	description := subscribeDescription(sc.Description, existingDescription, req)

	body := map[string]any{"description": description, "details": details}
	if meta, ok := existing["meta"].(map[string]any); ok && len(meta) > 0 {
		body["meta"] = meta
	}
	changed := description != existingDescription || !reflect.DeepEqual(details, existingDetails)
	return body, changed
}

func recordIdentity(data map[string]any) (recordID, state string) {
	recordID, _ = data["record_id"].(string)
	state, _ = data["state"].(string)
	return recordID, state
}

// doPost sends payload to a DeDi write endpoint and accepts any 2xx. Unlike
// doGet it keeps DeDi's response text in errors: Subscribe runs at startup,
// never on a request path where the text could leak into a NACK.
func (c *DeDiRegistryClient) doPost(ctx context.Context, url, operation string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return internalErr(fmt.Errorf("failed to encode %s request body: %w", operation, err))
	}
	httpReq, err := retryablehttp.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return internalErr(sanitize(ctx, fmt.Sprintf("DeDi %s failed: could not build request", operation),
			fmt.Errorf("failed to create %s request: %w", operation, err)))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.config.Subscribe.APIKey)
	httpReq = httpReq.WithContext(ctx)

	log.Debugf(ctx, "Making DeDi %s request to: %s", operation, url)
	resp, err := c.client.Do(httpReq)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return transportErr(ctx, operation, fmt.Errorf("failed to send DeDi %s request: %w", operation, err))
	}
	defer resp.Body.Close()

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyLogBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		statusErr := fmt.Errorf("DeDi %s request failed with status: %s, response: %s", operation, resp.Status, snippet)
		log.Errorf(ctx, nil, "%s", statusErr)
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden:
			return internalErr(fmt.Errorf("%w (DeDi rejected the credentials: check that %s holds a valid DeDi API key with write access to namespace %q)",
				statusErr, APIKeyEnv, c.config.Subscribe.Namespace))
		default:
			return classifyStatusCode(resp.StatusCode, statusErr, entityNotFound)
		}
	}
	log.Debugf(ctx, "DeDi %s succeeded with status %s: %s", operation, resp.Status, snippet)
	return nil
}
