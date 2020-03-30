package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricGoproxyLatestDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gobuild_goproxy_latest_duration_seconds",
			Help:    "Duration of request to goproxy to resolve latest module version in seconds.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
		},
	)
	metricGoproxyLatestErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_goproxy_latest_errors_total",
			Help: "Number of error reponses from goproxy for resolving latest module version, per http response code.",
		},
		[]string{"code"},
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
	metricPageErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_page_errors_total",
			Help: "Number of error reponses for serve page.",
		},
		[]string{"page", "error"},
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

	metricVerifyDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gobuild_verify_duration_seconds",
			Help:    "Duration of go build to compile program in seconds.",
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
)

func observePage(page string, t0 time.Time) {
	metricPageDuration.WithLabelValues(page).Observe(time.Since(t0).Seconds())
}
