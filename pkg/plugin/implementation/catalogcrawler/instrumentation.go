package catalogcrawler

// instrumentation.go — OpenTelemetry spans and plugin metrics for the
// crawler's own operations (registry lookups, poll/sync/park-sweep ticks,
// queue syncs, on-demand crawls). Purely additive to the slog logging these
// paths already do: every operation still logs exactly what it did before,
// it now also emits a span and records onix_plugin_execution_duration_seconds
// / onix_plugin_errors_total (pkg/telemetry) so crawler failures reach
// OTel-based dashboards and alerts, not only log aggregation.
//
// Everything here goes through the global otel TracerProvider/MeterProvider,
// like every other plugin (cache, registry, dediregistry, simplekeymanager):
// when telemetry isn't configured those are no-ops, so this costs next to
// nothing and never fails an operation.

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/telemetry"
	"github.com/beckn/catalog-core/pkg/catalog/crawler"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Metric identity labels (telemetry.AttrPluginID / AttrPluginType).
// pluginType matches the top-level `plugins.crawler` config slot, in the same
// snake_case style core/module/handler's PluginEntries uses for module slots.
const (
	pluginID   = "catalogcrawler"
	pluginType = "crawler"
)

// Operation names: the telemetry.AttrOperation metric label, and (prefixed
// with spanNamePrefix) the span name.
const (
	opPollTick       = "poll_tick"       // one scheduled Params.PollIndexes pass
	opSyncTick       = "sync_tick"       // one scheduled queue drain (span only -- its sync_next children carry the metrics)
	opSyncNext       = "sync_next"       // one Params.SyncNext call: claim the next queued item and settle it
	opParkSweep      = "park_sweep"      // one scheduled Params.RequeueOrAbandonParked pass
	opRegistryLookup = "registry_lookup" // one RegistryMetadataLookup.QueryByNetwork call for one network
	opOnDemandCrawl  = "on_demand_crawl" // one CrawlRegistry-triggered PollIndexes pass
)

const spanNamePrefix = "catalogcrawler."

// Outcome values for the telemetry.AttrStatus label.
const (
	statusSuccess = "success"
	statusError   = "error"
	// statusPartialFailure: the operation completed, but some of its
	// discovery inputs (registry networks or discovery sources) failed and
	// were skipped -- see registryDiscoverer.Discover / multiSource.Discover.
	statusPartialFailure = "partial_failure"
	// statusEmpty: a SyncNext call found nothing to claim (the normal end of
	// every queue drain), not a failure.
	statusEmpty = "empty"
)

// Values for the telemetry.AttrErrorType label. Deliberately a small fixed
// vocabulary (plus catalog-core's own bounded crawler.FaultClass values for a
// claimed item's sync failure) so the label stays low-cardinality -- error
// messages, network IDs and URLs only ever go on spans, never on metrics.
const (
	errTypeLookupFailed     = "lookup_failed"
	errTypePollFailed       = "poll_failed"
	errTypePartialDiscovery = "partial_discovery"
	errTypeClaimFailed      = "claim_failed"
	errTypeSweepFailed      = "sweep_failed"
	errTypeCanceled         = "canceled"
	errTypeTimeout          = "timeout"
)

// Span-only attribute keys (too high-cardinality for metric labels).
const (
	attrNetworkID      = attribute.Key("network_id")
	attrNetworkIDs     = attribute.Key("network_ids")
	attrProvidersFound = attribute.Key("providers_found")
	attrRunID          = attribute.Key("run_id")
	attrClaimed        = attribute.Key("claimed")
	attrItemsClaimed   = attribute.Key("items_claimed")
)

func tracer() trace.Tracer {
	return otel.Tracer(telemetry.ScopeName, trace.WithInstrumentationVersion(telemetry.ScopeVersion))
}

// operation is one instrumented unit of work: a span plus the start time its
// onix_plugin_execution_duration_seconds sample is measured from.
type operation struct {
	name  string
	span  trace.Span
	start time.Time
}

// startOperation starts op's span (named spanNamePrefix+op) as a child of
// whatever span ctx carries, unless opts say otherwise (e.g.
// trace.WithNewRoot for a scheduler tick).
func startOperation(ctx context.Context, op string, opts ...trace.SpanStartOption) (context.Context, *operation) {
	ctx, span := tracer().Start(ctx, spanNamePrefix+op, opts...)
	return ctx, &operation{name: op, span: span, start: time.Now()}
}

// end records the operation's outcome on its span and in the plugin metrics,
// then ends the span. errType is empty for an outcome that isn't a failure
// (success/empty); any non-empty errType also increments
// onix_plugin_errors_total. err, if non-nil, is recorded on the span.
func (o *operation) end(ctx context.Context, status, errType string, err error) {
	defer o.span.End()
	o.span.SetAttributes(telemetry.AttrStatus.String(status))
	if errType != "" {
		o.span.SetAttributes(telemetry.AttrErrorType.String(errType))
	}
	if err != nil {
		o.span.RecordError(err)
	}
	if status == statusError {
		desc := errType
		if err != nil {
			desc = err.Error()
		}
		o.span.SetStatus(codes.Error, desc)
	}
	recordOperation(ctx, o.name, status, errType, time.Since(o.start))
}

// endPoll settles a PollIndexes-driven operation (poll_tick, on_demand_crawl):
// a returned error is a failure; no error but a partial discovery failure
// flagged during the pass (see markPartialFailure) is a partial failure,
// still counted in onix_plugin_errors_total since the tick only carried on
// with whatever the healthy networks/sources returned.
func (o *operation) endPoll(ctx context.Context, err error, partial bool) {
	switch {
	case err != nil:
		o.end(ctx, statusError, classifyError(err, errTypePollFailed), err)
	case partial:
		o.end(ctx, statusPartialFailure, errTypePartialDiscovery, nil)
	default:
		o.end(ctx, statusSuccess, "", nil)
	}
}

// recordOperation records one completed operation: its duration (every
// outcome, labelled with its status so failure latencies can be told apart
// from successful ones) and, for a failure outcome, one error. Metrics are
// looked up per call, like the rest of pkg/telemetry's consumers, so they
// bind to whatever MeterProvider otelsetup registered; when GetMetrics
// fails, nothing is recorded -- telemetry must never fail the crawl itself.
func recordOperation(ctx context.Context, op, status, errType string, d time.Duration) {
	m, err := telemetry.GetMetrics(ctx)
	if err != nil || m == nil {
		return
	}
	attrs := []attribute.KeyValue{
		telemetry.AttrPluginID.String(pluginID),
		telemetry.AttrPluginType.String(pluginType),
		telemetry.AttrOperation.String(op),
		telemetry.AttrStatus.String(status),
	}
	m.PluginExecutionDuration.Record(ctx, d.Seconds(), metric.WithAttributes(attrs...))
	if errType != "" {
		errAttrs := make([]attribute.KeyValue, 0, len(attrs)+1)
		errAttrs = append(errAttrs, attrs...)
		errAttrs = append(errAttrs, telemetry.AttrErrorType.String(errType))
		m.PluginErrorsTotal.Add(ctx, 1, metric.WithAttributes(errAttrs...))
	}
}

// classifyError maps err onto the error_type label: context cancellation and
// deadline expiry get their own values (a shutdown-time cancel shouldn't
// look like a registry/store outage), anything else gets fallback.
func classifyError(err error, fallback string) string {
	switch {
	case errors.Is(err, context.Canceled):
		return errTypeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return errTypeTimeout
	default:
		return fallback
	}
}

// syncErrorType classifies a claimed queue item's sync failure by
// catalog-core's own bounded fault taxonomy (crawler.ClassifyFault: e.g.
// "signature", "digest_mismatch", "transient") -- the same
// permanent-vs-transient split SyncNext uses to decide park vs reschedule.
func syncErrorType(err error) string {
	if t := classifyError(err, ""); t != "" {
		return t
	}
	return string(crawler.ClassifyFault(0, err))
}

// partialFailureKey carries a per-pass *atomic.Bool through ctx so the
// discovery layers called from inside crawlmanager.Params.PollIndexes
// (registryDiscoverer, multiSource) can flag a tolerated partial failure
// back to the poll_tick/on_demand_crawl operation that owns the pass --
// PollIndexes itself only ever reports all-or-nothing.
type partialFailureKey struct{}

// withPartialFailureTracking returns a ctx that markPartialFailure can flag,
// and the flag to read once the pass is done.
func withPartialFailureTracking(ctx context.Context) (context.Context, *atomic.Bool) {
	flag := &atomic.Bool{}
	return context.WithValue(ctx, partialFailureKey{}, flag), flag
}

// markPartialFailure flags the enclosing pass (if ctx is tracking one) as
// partially failed, and adds an event to ctx's current span so the trace
// shows where. failed/total describe what failed, e.g. 1 of 3 networks.
func markPartialFailure(ctx context.Context, what string, failed, total int) {
	if flag, ok := ctx.Value(partialFailureKey{}).(*atomic.Bool); ok {
		flag.Store(true)
	}
	trace.SpanFromContext(ctx).AddEvent("partial discovery failure", trace.WithAttributes(
		attribute.String("what", what),
		attribute.Int("failed", failed),
		attribute.Int("total", total),
	))
}
