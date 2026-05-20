package metrics

import (
	"net/http"
	"strconv"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry         *prometheus.Registry
	APIRequestsTotal *prometheus.CounterVec
	QueueDepth       prometheus.Gauge
	WorkerJobsTotal  *prometheus.CounterVec
}

func New() *Metrics {
	registry := prometheus.NewRegistry()
	m := &Metrics{
		registry: registry,
		APIRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "api_requests_total",
			Help: "Total HTTP API requests.",
		}, []string{"method", "status"}),
		QueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "queue_depth",
			Help: "Current background job queue depth.",
		}),
		WorkerJobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "worker_jobs_total",
			Help: "Total worker jobs processed.",
		}, []string{"type", "status"}),
	}

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.APIRequestsTotal,
		m.QueueDepth,
		m.WorkerJobsTotal,
	)

	return m
}

func (m *Metrics) Middleware(c fiber.Ctx) error {
	err := c.Next()
	status := c.Response().StatusCode()
	m.APIRequestsTotal.WithLabelValues(c.Method(), strconv.Itoa(status)).Inc()
	return err
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}
