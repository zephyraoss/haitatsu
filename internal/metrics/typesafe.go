package metrics

import "github.com/prometheus/client_golang/prometheus"

func (m *Metrics) initTypeSafe() {
	m.TypeSafeAssessments = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "typesafe_assessments_total", Help: "TypeSafe assessment outcomes, including skips."}, []string{"mode", "status"})
	m.TypeSafeLatency = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "typesafe_request_duration_seconds", Help: "TypeSafe evaluation duration.", Buckets: []float64{.01, .05, .1, .25, .5, 1, 2, 5, 10}})
	m.TypeSafeProbabilities = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "typesafe_probability", Help: "Successful TypeSafe probabilities.", Buckets: []float64{0, .1, .5, .9, .95, .98, .99, 1}}, []string{"question"})
	m.TypeSafeTokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "typesafe_tokens_total", Help: "Reported TypeSafe token usage."}, []string{"direction"})
	m.TypeSafeDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "typesafe_junk_decisions_total", Help: "Proposed and applied TypeSafe Junk decisions."}, []string{"mode", "decision"})
	m.registry.MustRegister(m.TypeSafeAssessments, m.TypeSafeLatency, m.TypeSafeProbabilities, m.TypeSafeTokens, m.TypeSafeDecisions)
}
