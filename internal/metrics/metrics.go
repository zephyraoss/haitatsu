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
	registry               *prometheus.Registry
	APIRequestsTotal       *prometheus.CounterVec
	QueueDepth             prometheus.Gauge
	WorkerJobsTotal        *prometheus.CounterVec
	SMTPConnectionsTotal   prometheus.Counter
	MessagesReceivedTotal  prometheus.Counter
	MessagesSentTotal      prometheus.Counter
	MessagesDeliveredTotal prometheus.Counter
	MessagesBouncedTotal   prometheus.Counter
	MailboxOverQuotaTotal  prometheus.Counter
	WebhookFailuresTotal   prometheus.Counter
	IMAPSessions           prometheus.Gauge
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
		SMTPConnectionsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "smtp_connections_total",
			Help: "Total inbound SMTP connections.",
		}),
		MessagesReceivedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "messages_received_total",
			Help: "Total inbound messages received.",
		}),
		MessagesSentTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "messages_sent_total",
			Help: "Total outbound messages sent.",
		}),
		MessagesDeliveredTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "messages_delivered_total",
			Help: "Total mailbox deliveries.",
		}),
		MessagesBouncedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "messages_bounced_total",
			Help: "Total inbound bounces recorded.",
		}),
		MailboxOverQuotaTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mailbox_over_quota_total",
			Help: "Total over-quota recipient rejections.",
		}),
		WebhookFailuresTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "webhook_failures_total",
			Help: "Total webhook delivery failures.",
		}),
		IMAPSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "imap_sessions",
			Help: "Current active IMAP sessions.",
		}),
	}

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.APIRequestsTotal,
		m.QueueDepth,
		m.WorkerJobsTotal,
		m.SMTPConnectionsTotal,
		m.MessagesReceivedTotal,
		m.MessagesSentTotal,
		m.MessagesDeliveredTotal,
		m.MessagesBouncedTotal,
		m.MailboxOverQuotaTotal,
		m.WebhookFailuresTotal,
		m.IMAPSessions,
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

func (m *Metrics) SMTPConnection()            { m.SMTPConnectionsTotal.Inc() }
func (m *Metrics) MessageReceived()           { m.MessagesReceivedTotal.Inc() }
func (m *Metrics) MessageSent()               { m.MessagesSentTotal.Inc() }
func (m *Metrics) MessageBounced()            { m.MessagesBouncedTotal.Inc() }
func (m *Metrics) MailboxOverQuota()          { m.MailboxOverQuotaTotal.Inc() }
func (m *Metrics) WebhookFailure()            { m.WebhookFailuresTotal.Inc() }
func (m *Metrics) IMAPSessionStart()          { m.IMAPSessions.Inc() }
func (m *Metrics) IMAPSessionEnd()            { m.IMAPSessions.Dec() }
func (m *Metrics) MessageDelivered(count int) { m.MessagesDeliveredTotal.Add(float64(count)) }
func (m *Metrics) SetQueueDepth(count int)    { m.QueueDepth.Set(float64(count)) }
func (m *Metrics) WorkerJob(jobType string, status string) {
	m.WorkerJobsTotal.WithLabelValues(jobType, status).Inc()
}
