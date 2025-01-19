package main

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricPanics = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "mox_panics_total",
			Help: "Number of unhandled panics.",
		},
	)
	metricGoproxyResolveVersionDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gobuild_goproxy_resolve_version_duration_seconds",
			Help:    "Duration of request to goproxy to resolve module version in seconds.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
		},
	)

	metricGoproxyListDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gobuild_goproxy_list_duration_seconds",
			Help:    "Duration of request to goproxy to list module versions in seconds.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
		},
	)
	metricGoproxyListErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_goproxy_list_errors_total",
			Help: "Number of error reponses from goproxy for listing module versions, per http response code.",
		},
		[]string{"code"},
	)

	metricPageDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gobuild_page_duration_seconds",
			Help:    "Duration of request for page in seconds, per http response code.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
		},
		[]string{"page"},
	)

	metricGogetDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gobuild_goget_duration_seconds",
			Help:    "Duration of go get to fetch module source in seconds.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
		},
	)
	metricGogetErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gobuild_goget_errors_total",
			Help: "Number of error reponses during go get.",
		},
	)
	metricListPackageErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gobuild_list_package_errors_total",
			Help: "Number of errors listing packages.",
		},
	)
	metricNotMainErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gobuild_not_main_errors_total",
			Help: "Number of errors due to requested package not being main.",
		},
	)
	metricCheckCgoErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gobuild_check_cgo_errors_total",
			Help: "Number of errors while checking if module needs cgo.",
		},
	)
	metricNeedsCgoErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gobuild_needs_cgo_errors_total",
			Help: "Number of errors due to package needing cgo.",
		},
	)
	metricResolveVersionErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gobuild_resolve_version_errors_total",
			Help: "Number of errors while resolving version for module.",
		},
	)

	metricCompileDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gobuild_compile_duration_seconds",
			Help:    "Duration of go build to compile program in seconds.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128, 256},
		},
		[]string{"goos", "goarch", "goversion"},
	)
	metricCompileErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_compile_errors_total",
			Help: "Number of error reponses during go build.",
		},
		[]string{"goos", "goarch", "goversion"},
	)

	metricRecompileMismatch = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_recompile_mismatch_total",
			Help: "Number of sum mismatches when recompiling cleaned up binaries.",
		},
		[]string{"goos", "goarch", "goversion"},
	)

	metricVerifyDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gobuild_verify_duration_seconds",
			Help:    "Duration of verifying build with other backend, in seconds.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128, 256},
		},
		[]string{"baseurl", "goos", "goarch", "goversion"},
	)
	metricVerifyErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_verify_errors_total",
			Help: "Number of error reponses verifying with other backends.",
		},
		[]string{"baseurl", "goos", "goarch", "goversion"},
	)
	metricVerifyMismatch = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_verify_mismatch_total",
			Help: "Number of sum mismatches with other backends.",
		},
		[]string{"baseurl", "goos", "goarch", "goversion"},
	)

	metricTlogAddErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gobuild_tlog_add_errors_total",
			Help: "Number of errors (of any kind, including consistency) while adding a sum to the transparency log.",
		},
	)
	metricTlogConsistencyErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "gobuild_tlog_consistency_errors_total",
			Help: "Number of consistency errors encountered while adding a sum to the transparency log.",
		},
	)
	metricTlogRecords = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "gobuild_tlog_record_total",
			Help: "Number of records in the transparency log.",
		},
	)

	metricTlogOpsSignedErrors       = newOpsErrorCounter("signed")
	metricTlogOpsReadrecordsErrors  = newOpsErrorCounter("readrecords")
	metricTlogOpsLookupErrors       = newOpsErrorCounter("lookup")
	metricTlogOpsReadtiledataErrors = newOpsErrorCounter("readtiledata")

	metricTlogOpsSignedDuration       = newOpsHistogram("signed")
	metricTlogOpsReadrecordsDuration  = newOpsHistogram("readrecords")
	metricTlogOpsLookupDuration       = newOpsHistogram("lookup")
	metricTlogOpsReadtiledataDuration = newOpsHistogram("readtiledata")
)

func newOpsErrorCounter(op string) prometheus.Counter {
	return promauto.NewCounter(
		prometheus.CounterOpts{
			Name: fmt.Sprintf("gobuild_tlog_ops_%s_errors_total", op),
			Help: fmt.Sprintf("Number of transparency log errors for server op %s on the transparency log.", op),
		},
	)
}

func newOpsHistogram(op string) prometheus.Histogram {
	return promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    fmt.Sprintf("gobuild_tlog_ops_%s_duration_seconds", op),
			Help:    fmt.Sprintf("Duration of transparency log server op %s in seconds.", op),
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1},
		},
	)
}

func observePage(page string, t0 time.Time) {
	metricPageDuration.WithLabelValues(page).Observe(time.Since(t0).Seconds())
}
