package app

import (
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The four golden signals for this service's HTTP surface.
//
// Metric and label names deliberately match the cluster's existing
// `go-fiber-dashboard.json` (`grafana-dashboards-ext`, from
// cluster-helm-charts/charts/dimo-mon), which queries `http_requests_total` and
// `http_request_duration_seconds_bucket` by `method`, `path` and `status`. That
// dashboard has matched no DIMO Go service until now — only dex emits these
// names — so following it costs nothing and lights it up for free, where
// inventing names would leave it broken for another year.
//
//	latency     http_request_duration_seconds  (histogram)
//	traffic     http_requests_total            (counter)
//	errors      http_requests_total{status=5xx}
//	saturation  http_requests_in_flight        (gauge)
var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests, by method, route pattern and status code.",
	}, []string{"method", "path", "status"})

	// Buckets are tuned for THIS service rather than left at the client
	// default. /v1/authz is two indexed reads on an in-cluster call with no
	// ingress in front of it — the interesting range is single-digit
	// milliseconds, and the default 5ms/10ms/25ms spacing puts almost every
	// request in the first bucket, which makes a p99 unreadable exactly where
	// it matters. The long tail is kept for the paths that mint a DIMO token or
	// wait on accounts-api.
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, by method, route pattern and status code.",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"method", "path", "status"})

	requestsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "http_requests_in_flight",
		Help: "HTTP requests currently being served.",
	})
)

// NewMetricsMiddleware records one observation per request.
//
// THE LABEL IS THE ROUTE PATTERN, NEVER THE URL. `c.Route().Path` gives
// `/v1/tenants/:tenantId/vehicles`; `c.Path()` would give a distinct label
// value per tenant uuid, so a few hundred customers would become a few hundred
// series per method per status — the classic way a metrics endpoint becomes the
// most expensive thing a service does. Unmatched requests collapse to a single
// "unmatched" bucket for the same reason: a 404 is usually a scanner walking
// paths, and each miss would otherwise mint a permanent series.
func NewMetricsMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		requestsInFlight.Inc()
		defer requestsInFlight.Dec()

		// Run the rest of the chain first: the error handler converts a
		// returned error into a status code, and observing before that would
		// record every failure as a 200.
		err := c.Next()

		status := c.Response().StatusCode()
		if err != nil {
			// The handler returned an error the ErrorHandler has not written
			// yet. Read the status it will use, so a 503 is not filed as 200.
			var fe *fiber.Error
			if errors.As(err, &fe) {
				status = fe.Code
			} else {
				status = fiber.StatusInternalServerError
			}
		}

		labels := prometheus.Labels{
			"method": c.Method(),
			"path":   routeLabel(c),
			"status": strconv.Itoa(status),
		}
		requestsTotal.With(labels).Inc()
		requestDuration.With(labels).Observe(time.Since(start).Seconds())
		return err
	}
}

// routeLabel is the registered route pattern, or "unmatched".
//
// A REQUEST REFUSED AT THE /v1 GATE IS LABELLED "/v1", NOT ITS REAL ROUTE, and
// that is worth knowing before it is filed as a bug. The trusted-caller guard
// and the JWT middleware are mounted on the group, so when one of them aborts,
// the last route fiber executed is the group's own mount path. Verified in
// prod: six unauthenticated calls to /v1/authz and
// /v1/tenants/{id}/vehicles all recorded as
//
//	http_requests_total{method="GET",path="/v1",status="401"} 6
//
// It reads oddly and it is the useful behaviour: everything turned away before
// reaching a handler collapses into one series, so "requests refused at the
// gate" is a single line on a chart rather than smeared across every route.
// Requests that reach their handler carry their real pattern.
func routeLabel(c *fiber.Ctx) string {
	if r := c.Route(); r != nil && r.Path != "" {
		// Fiber reports "/" for unmatched paths on some versions; treat a "/"
		// route label on a non-root request as unmatched rather than merging
		// scanner traffic into the root route's numbers.
		if r.Path == "/" && c.Path() != "/" {
			return "unmatched"
		}
		return r.Path
	}
	return "unmatched"
}
