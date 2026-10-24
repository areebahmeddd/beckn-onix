package dediregistry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
	"github.com/beckn-one/beckn-onix/pkg/testutil"
)

const testAPIKey = "test-api-key"

// fakeDeDi is an in-memory stand-in for the DeDi API Subscribe talks to: the
// public record lookup and the portal's create and update calls. It records
// every request and lets a test override any response.
type fakeDeDi struct {
	t *testing.T

	mu       sync.Mutex
	record   map[string]any // the record's lookup "data" object; nil means none exists
	requests []fakeRequest

	// When set, these replace the default handling of the matching call.
	lookup func(w http.ResponseWriter, r *http.Request) bool // returns true when it wrote a response
	write  http.HandlerFunc
}

type fakeRequest struct {
	method, path, rawQuery, auth string
	body                         map[string]any
}

func newFakeDeDi(t *testing.T) (*fakeDeDi, *httptest.Server) {
	t.Helper()
	f := &fakeDeDi{t: t}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeDeDi) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	req := fakeRequest{method: r.Method, path: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req.body); err != nil {
			f.t.Errorf("request body is not JSON: %v", err)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			f.t.Errorf("Content-Type = %q, want application/json", ct)
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()

	if r.Method == http.MethodGet {
		if f.lookup != nil && f.lookup(w, r) {
			return
		}
		f.mu.Lock()
		record := f.record
		f.mu.Unlock()
		if record == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Record not found"}`)) //nolint:errcheck
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"message": "Resource retrieved successfully", "data": record}) //nolint:errcheck
		return
	}

	if f.write != nil {
		f.write(w, r)
		return
	}
	if req.auth != "Bearer "+testAPIKey {
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"message":"Payment Required"}`)) //nolint:errcheck
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/save-record-as-draft"):
		f.record = map[string]any{
			"record_id":   "rec-id-1",
			"record_name": req.body["record_name"],
			"description": req.body["description"],
			"details":     req.body["details"],
			"state":       "live",
		}
	case strings.HasSuffix(r.URL.Path, "/update-record"):
		f.record["description"] = req.body["description"]
		f.record["details"] = req.body["details"]
		if meta, ok := req.body["meta"]; ok {
			f.record["meta"] = meta
		}
	}
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(`{"message":"ok"}`)) //nolint:errcheck
}

// setRecord stores a record by round-tripping it through JSON, so its values
// have the types a real lookup response decodes into.
func (f *fakeDeDi) setRecord(record map[string]any) {
	f.t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		f.t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.record = decoded
	f.mu.Unlock()
}

// all returns every request received.
func (f *fakeDeDi) all() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

// writes returns the non-GET requests received.
func (f *fakeDeDi) writes() []fakeRequest {
	var out []fakeRequest
	for _, r := range f.all() {
		if r.method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func subscribeTestConfig(serverURL string) *Config {
	cfg := fastRetryConfig(serverURL)
	cfg.Subscribe = &SubscribeConfig{
		URL:          serverURL + "/dedi/",
		Namespace:    "np.example.com",
		RegistryName: "nfo-x_mobility_prod_registry",
		RecordName:   "bap-record",
		Countries:    []string{"IND"},
		APIKey:       testAPIKey,
	}
	return cfg
}

func testSubscription() *model.Subscription {
	return &model.Subscription{
		Subscriber: model.Subscriber{
			SubscriberID: "np.example.com",
			URL:          "https://np.example.com/bap/receiver",
			Type:         "BAP",
		},
		KeyID:            "rec-id-1",
		SigningPublicKey: "c2lnbmluZy1wdWJsaWMta2V5",
		EncrPublicKey:    "ZW5jci1wdWJsaWMta2V5",
	}
}

// wantDetails is the details object testSubscription and subscribeTestConfig
// publish, in its decoded-JSON form.
func wantDetails() map[string]any {
	return map[string]any{
		"subscriber_id":      "np.example.com",
		"url":                "https://np.example.com/bap/receiver",
		"type":               "BAP",
		"signing_public_key": "c2lnbmluZy1wdWJsaWMta2V5",
		"encr_public_key":    "ZW5jci1wdWJsaWMta2V5",
		"countries":          []any{"IND"},
	}
}

func TestSubscribeCreatesMissingRecord(t *testing.T) {
	f, srv := newFakeDeDi(t)
	client := newTestClient(t, subscribeTestConfig(srv.URL))

	result, err := client.Subscribe(context.Background(), testSubscription())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	want := &model.SubscribeResult{Status: model.SubscribeCreated, RecordID: "rec-id-1", State: "live"}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("Subscribe() = %+v, want %+v", result, want)
	}

	writes := f.writes()
	if len(writes) != 1 {
		t.Fatalf("expected 1 write, got %d: %+v", len(writes), writes)
	}
	w := writes[0]
	if w.method != http.MethodPost || w.path != "/dedi/np.example.com/nfo-x_mobility_prod_registry/save-record-as-draft" || w.rawQuery != "publish=true" {
		t.Errorf("create request = %s %s?%s", w.method, w.path, w.rawQuery)
	}
	if w.auth != "Bearer "+testAPIKey {
		t.Errorf("Authorization = %q, want the bearer API key", w.auth)
	}
	if w.body["record_name"] != "bap-record" {
		t.Errorf("record_name = %v", w.body["record_name"])
	}
	if w.body["description"] != "Beckn subscriber record for np.example.com (BAP)" {
		t.Errorf("description = %v, want the generated default", w.body["description"])
	}
	if !reflect.DeepEqual(w.body["details"], wantDetails()) {
		t.Errorf("details = %v, want %v", w.body["details"], wantDetails())
	}
	if _, ok := w.body["meta"]; ok {
		t.Error("create must not send meta")
	}
	for _, r := range f.all() {
		if r.method == http.MethodGet {
			if r.path != "/dedi/lookup/np.example.com/nfo-x_mobility_prod_registry/bap-record" {
				t.Errorf("lookup path = %s", r.path)
			}
			if r.auth != "" {
				t.Errorf("the public lookup must not carry the API key, got %q", r.auth)
			}
		}
	}
}

func TestSubscribeOmitsOptionalDetails(t *testing.T) {
	f, srv := newFakeDeDi(t)
	client := newTestClient(t, subscribeTestConfig(srv.URL))
	req := testSubscription()
	req.EncrPublicKey = ""
	req.Domain = "mobility"

	if _, err := client.Subscribe(context.Background(), req); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	details := f.writes()[0].body["details"].(map[string]any)
	if _, ok := details["encr_public_key"]; ok {
		t.Error("an empty encryption key must not be published")
	}
	if details["domain"] != "mobility" {
		t.Errorf("domain = %v, want mobility", details["domain"])
	}
}

func TestSubscribeUpdatesChangedRecord(t *testing.T) {
	f, srv := newFakeDeDi(t)
	existingDetails := wantDetails()
	existingDetails["url"] = "https://old.example.com/bap/receiver"
	existingDetails["extra_field"] = "kept"
	f.setRecord(map[string]any{
		"record_id":   "rec-id-1",
		"record_name": "bap-record",
		"description": "operator description",
		"details":     existingDetails,
		"meta":        map[string]any{"catalog_index_urls": []any{map[string]any{"url": "https://np.example.com/index.json"}}},
		"state":       "live",
	})
	client := newTestClient(t, subscribeTestConfig(srv.URL))

	result, err := client.Subscribe(context.Background(), testSubscription())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if result.Status != model.SubscribeUpdated || result.RecordID != "rec-id-1" || result.State != "live" {
		t.Errorf("Subscribe() = %+v", result)
	}

	writes := f.writes()
	if len(writes) != 1 {
		t.Fatalf("expected 1 write, got %d", len(writes))
	}
	w := writes[0]
	if w.path != "/dedi/np.example.com/nfo-x_mobility_prod_registry/bap-record/update-record" || w.rawQuery != "" {
		t.Errorf("update request = %s %s?%s", w.method, w.path, w.rawQuery)
	}
	if w.auth != "Bearer "+testAPIKey {
		t.Errorf("Authorization = %q", w.auth)
	}
	details := wantDetails()
	details["extra_field"] = "kept"
	if !reflect.DeepEqual(w.body["details"], details) {
		t.Errorf("details = %v, want %v", w.body["details"], details)
	}
	if w.body["description"] != "operator description" {
		t.Errorf("description = %v, want the existing one kept", w.body["description"])
	}
	if _, ok := w.body["meta"]; !ok {
		t.Error("update must send the existing meta back so it is not dropped")
	}
	if _, ok := w.body["record_name"]; ok {
		t.Error("update must not send record_name")
	}
}

func TestSubscribeUpdatesConfiguredDescription(t *testing.T) {
	f, srv := newFakeDeDi(t)
	f.setRecord(map[string]any{"record_id": "rec-id-1", "description": "old", "details": wantDetails(), "state": "live"})
	cfg := subscribeTestConfig(srv.URL)
	cfg.Subscribe.Description = "new"
	client := newTestClient(t, cfg)

	result, err := client.Subscribe(context.Background(), testSubscription())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if result.Status != model.SubscribeUpdated {
		t.Errorf("Status = %s, want updated", result.Status)
	}
	if got := f.writes()[0].body["description"]; got != "new" {
		t.Errorf("description = %v, want new", got)
	}
}

func TestSubscribeLeavesMatchingRecordAlone(t *testing.T) {
	f, srv := newFakeDeDi(t)
	f.setRecord(map[string]any{"record_id": "rec-id-1", "description": "kept", "details": wantDetails(), "state": "draft"})
	client := newTestClient(t, subscribeTestConfig(srv.URL))

	result, err := client.Subscribe(context.Background(), testSubscription())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	want := &model.SubscribeResult{Status: model.SubscribeUnchanged, RecordID: "rec-id-1", State: "draft"}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("Subscribe() = %+v, want %+v", result, want)
	}
	if writes := f.writes(); len(writes) != 0 {
		t.Errorf("expected no writes for an up-to-date record, got %+v", writes)
	}
}

func TestSubscribeRecordWithoutDetailsIsUpdated(t *testing.T) {
	f, srv := newFakeDeDi(t)
	f.setRecord(map[string]any{"record_id": "rec-id-1", "state": "live"})
	client := newTestClient(t, subscribeTestConfig(srv.URL))

	result, err := client.Subscribe(context.Background(), testSubscription())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if result.Status != model.SubscribeUpdated {
		t.Errorf("Status = %s, want updated", result.Status)
	}
	if !reflect.DeepEqual(f.writes()[0].body["details"], wantDetails()) {
		t.Errorf("details = %v", f.writes()[0].body["details"])
	}
}

func TestSubscribeEscapesPathSegments(t *testing.T) {
	f, srv := newFakeDeDi(t)
	cfg := subscribeTestConfig(srv.URL)
	cfg.Subscribe.RecordName = "bap record?x"
	client := newTestClient(t, cfg)

	if _, err := client.Subscribe(context.Background(), testSubscription()); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if got := f.all()[0].path; got != "/dedi/lookup/np.example.com/nfo-x_mobility_prod_registry/bap%20record%3Fx" {
		t.Errorf("lookup path = %s", got)
	}
	if got := f.writes()[0].body["record_name"]; got != "bap record?x" {
		t.Errorf("record_name = %v", got)
	}
}

func TestSubscribeReadBackFailureStillSucceeds(t *testing.T) {
	f, srv := newFakeDeDi(t)
	var lookups atomic.Int32
	f.lookup = func(w http.ResponseWriter, _ *http.Request) bool {
		if lookups.Add(1) == 1 {
			return false // the record is missing, so it is created
		}
		w.Write([]byte("not json")) //nolint:errcheck
		return true
	}
	client := newTestClient(t, subscribeTestConfig(srv.URL))

	result, err := client.Subscribe(context.Background(), testSubscription())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	want := &model.SubscribeResult{Status: model.SubscribeCreated}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("Subscribe() = %+v, want %+v", result, want)
	}
}

func TestSubscribeWriteFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"Unauthorized"}`, http.StatusInternalServerError, codeNetInternalError, APIKeyEnv},
		{"payment required (no API key)", http.StatusPaymentRequired, `{"message":"Payment Required"}`, http.StatusInternalServerError, codeNetInternalError, APIKeyEnv},
		{"forbidden", http.StatusForbidden, `{"message":"Forbidden"}`, http.StatusInternalServerError, codeNetInternalError, `namespace "np.example.com"`},
		{"schema rejected", http.StatusBadRequest, `{"message":"Record data does not match the registry schema","errors":[{"keyword":"required","params":{"missingProperty":"countries"}}]}`, http.StatusInternalServerError, codeNetInternalError, "missingProperty"},
		{"namespace or registry missing", http.StatusNotFound, `{"message":"Registry not found"}`, http.StatusNotFound, codeNetEntityNotFound, "Registry not found"},
		{"server error after retries", http.StatusInternalServerError, `oops`, http.StatusServiceUnavailable, codeNetDownstreamUnavailable, "oops"},
		{"unexpected redirect status", http.StatusNotModified, ``, http.StatusServiceUnavailable, codeNetDownstreamUnavailable, "304"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, srv := newFakeDeDi(t)
			f.write = rawHandler(tt.status, tt.body)
			client := newTestClient(t, subscribeTestConfig(srv.URL))

			result, err := client.Subscribe(context.Background(), testSubscription())
			if result != nil {
				t.Errorf("expected no result on failure, got %+v", result)
			}
			testutil.RequireCodedErr(t, err, tt.wantStatus, tt.wantCode)
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tt.wantMsg)
			}
		})
	}
}

func TestSubscribeRetriesTransientWriteFailure(t *testing.T) {
	f, srv := newFakeDeDi(t)
	var attempts atomic.Int32
	f.write = func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
	client := newTestClient(t, subscribeTestConfig(srv.URL))

	result, err := client.Subscribe(context.Background(), testSubscription())
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if result.Status != model.SubscribeCreated {
		t.Errorf("Status = %s, want created", result.Status)
	}
	writes := f.writes()
	if len(writes) != 2 {
		t.Fatalf("expected the create to be retried once, got %d writes", len(writes))
	}
	if !reflect.DeepEqual(writes[0].body, writes[1].body) {
		t.Error("the retried request must resend the same body")
	}
}

func TestSubscribeLookupFailures(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus int
		wantCode   string
	}{
		{"not JSON", rawHandler(http.StatusOK, "not json"), http.StatusBadGateway, codeNetDownstreamInvalidResp},
		{"no data", rawHandler(http.StatusOK, `{"message":"ok"}`), http.StatusBadGateway, codeNetDownstreamInvalidResp},
		{"server error", statusHandler(http.StatusInternalServerError), http.StatusServiceUnavailable, codeNetDownstreamUnavailable},
		{"rejected", statusHandler(http.StatusBadRequest), http.StatusInternalServerError, codeNetInternalError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, srv := newFakeDeDi(t)
			f.lookup = func(w http.ResponseWriter, r *http.Request) bool {
				tt.handler(w, r)
				return true
			}
			client := newTestClient(t, subscribeTestConfig(srv.URL))

			_, err := client.Subscribe(context.Background(), testSubscription())
			testutil.RequireCodedErr(t, err, tt.wantStatus, tt.wantCode)
			if writes := f.writes(); len(writes) != 0 {
				t.Errorf("expected no writes after a failed lookup, got %d", len(writes))
			}
		})
	}
}

func TestSubscribeTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	client := newTestClient(t, subscribeTestConfig(srv.URL))

	_, err := client.Subscribe(context.Background(), testSubscription())
	testutil.RequireCodedErr(t, err, http.StatusServiceUnavailable, codeNetDownstreamUnavailable)
}

func TestSubscribeWriteTransportFailure(t *testing.T) {
	f, srv := newFakeDeDi(t)
	f.write = func(w http.ResponseWriter, _ *http.Request) {
		// Hijack and drop the connection so the client sees a transport error.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	client := newTestClient(t, subscribeTestConfig(srv.URL))

	_, err := client.Subscribe(context.Background(), testSubscription())
	testutil.RequireCodedErr(t, err, http.StatusServiceUnavailable, codeNetDownstreamUnavailable)
}

func TestSubscribeCancelledContext(t *testing.T) {
	_, srv := newFakeDeDi(t)
	client := newTestClient(t, subscribeTestConfig(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Subscribe(ctx, testSubscription())
	testutil.RequireCodedErr(t, err, http.StatusInternalServerError, codeNetRequestCancelled)
}

func TestSubscribeRejectsInvalidInput(t *testing.T) {
	_, srv := newFakeDeDi(t)
	client := newTestClient(t, subscribeTestConfig(srv.URL))

	t.Run("nil subscription", func(t *testing.T) {
		_, err := client.Subscribe(context.Background(), nil)
		testutil.RequireCodedErr(t, err, http.StatusBadRequest, codeCtxMissingField)
	})
	for _, field := range []string{"subscriber_id", "url", "type", "signing_public_key"} {
		t.Run("missing "+field, func(t *testing.T) {
			req := testSubscription()
			switch field {
			case "subscriber_id":
				req.SubscriberID = " "
			case "url":
				req.URL = ""
			case "type":
				req.Type = ""
			case "signing_public_key":
				req.SigningPublicKey = ""
			}
			_, err := client.Subscribe(context.Background(), req)
			testutil.RequireCodedErr(t, err, http.StatusBadRequest, codeCtxMissingField)
			if !strings.Contains(err.Error(), field) {
				t.Errorf("error %q does not name %s", err, field)
			}
		})
	}
}

func TestSubscribeNotConfigured(t *testing.T) {
	client := newTestClient(t, &Config{URL: "https://registry.example.com/dedi"})

	_, err := client.Subscribe(context.Background(), testSubscription())
	testutil.RequireCodedErr(t, err, http.StatusInternalServerError, codeNetInternalError)
	if !strings.Contains(err.Error(), "subscribeURL") {
		t.Errorf("error %q should say how to enable subscribe", err)
	}
}

func TestValidateSubscribe(t *testing.T) {
	valid := func() *SubscribeConfig {
		return &SubscribeConfig{
			URL: "https://api.dedi.global/dedi", Namespace: "np.example.com", RegistryName: "reg",
			RecordName: "rec", Countries: []string{"IND"}, APIKey: "k",
		}
	}
	tests := []struct {
		name    string
		mutate  func(*SubscribeConfig)
		wantErr string
	}{
		{"valid", func(*SubscribeConfig) {}, ""},
		{"empty url", func(s *SubscribeConfig) { s.URL = "" }, "subscribeURL cannot be empty"},
		{"relative url", func(s *SubscribeConfig) { s.URL = "api.dedi.global/dedi" }, "subscribeURL must be an absolute http(s) URL"},
		{"url with query", func(s *SubscribeConfig) { s.URL = "https://api.dedi.global/dedi?x=1" }, "subscribeURL must not have a query"},
		{"no API key", func(s *SubscribeConfig) { s.APIKey = "" }, APIKeyEnv},
		{"empty namespace", func(s *SubscribeConfig) { s.Namespace = "" }, "subscribeNamespace cannot be empty"},
		{"dot registry", func(s *SubscribeConfig) { s.RegistryName = ".." }, "subscribeRegistryName"},
		{"record with slash", func(s *SubscribeConfig) { s.RecordName = "a/b" }, "subscribeRecordName"},
		{"blank record", func(s *SubscribeConfig) { s.RecordName = "  " }, "subscribeRecordName"},
		{"no countries", func(s *SubscribeConfig) { s.Countries = nil }, "subscribeCountries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := valid()
			tt.mutate(s)
			err := validateSubscribe(s)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateSubscribe() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateSubscribe() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}

	t.Run("New rejects an invalid subscribe config", func(t *testing.T) {
		cfg := &Config{URL: "https://registry.example.com/dedi", Subscribe: valid()}
		cfg.Subscribe.APIKey = ""
		if _, _, err := New(context.Background(), nil, cfg); err == nil || !strings.Contains(err.Error(), APIKeyEnv) {
			t.Fatalf("New() error = %v, want the missing API key", err)
		}
	})
}

func TestNewCopiesSubscribeConfig(t *testing.T) {
	cfg := subscribeTestConfig("http://dedi.example.com")
	client := newTestClient(t, cfg)

	if got := client.config.Subscribe.URL; got != "http://dedi.example.com/dedi" {
		t.Errorf("subscribe URL = %q, want the trailing slash trimmed", got)
	}
	if cfg.Subscribe.URL != "http://dedi.example.com/dedi/" {
		t.Error("New must not modify the caller's config")
	}
	cfg.Subscribe.Countries[0] = "USA"
	if client.config.Subscribe.Countries[0] != "IND" {
		t.Error("the client must not share the caller's Countries slice")
	}
}

func TestSubscribeConfigNeverLogsAPIKey(t *testing.T) {
	cfg := subscribeTestConfig("http://dedi.example.com")
	for _, s := range []string{
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%v", *cfg),
		fmt.Sprintf("%+v", cfg.Subscribe),
		fmt.Sprint(*cfg.Subscribe),
	} {
		if strings.Contains(s, testAPIKey) {
			t.Errorf("formatted config exposes the API key: %s", s)
		}
	}
	if s := cfg.Subscribe.String(); !strings.Contains(s, "APIKeySet:true") || !strings.Contains(s, "RecordName:bap-record") {
		t.Errorf("String() = %s", s)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testAPIKey) {
		t.Errorf("JSON-encoded config exposes the API key: %s", raw)
	}
}

func TestRecordUpdateComparesDecodedValues(t *testing.T) {
	sc := subscribeTestConfig("http://dedi.example.com").Subscribe
	existing := map[string]any{"description": "d", "details": wantDetails()}
	if _, changed := recordUpdate(sc, testSubscription(), existing); changed {
		t.Error("identical details must not count as a change")
	}
	existing["details"].(map[string]any)["countries"] = []any{"IND", "USA"}
	if _, changed := recordUpdate(sc, testSubscription(), existing); !changed {
		t.Error("different countries must count as a change")
	}
}

// Subscribe must not hang past the client timeout when DeDi stops responding.
func TestSubscribeTimeout(t *testing.T) {
	f, srv := newFakeDeDi(t)
	f.lookup = func(w http.ResponseWriter, r *http.Request) bool {
		blockingHandler(w, r)
		return true
	}
	client := newTestClient(t, subscribeTestConfig(srv.URL))
	client.client.HTTPClient.Timeout = 50 * time.Millisecond

	_, err := client.Subscribe(context.Background(), testSubscription())
	testutil.RequireCodedErr(t, err, http.StatusGatewayTimeout, codeNetTimeout)
}
