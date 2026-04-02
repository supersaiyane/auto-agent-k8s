package obs

import "github.com/prometheus/client_golang/prometheus"

var (
	ActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "auto_agent_actions_total",
			Help: "Count of remediation actions by type, namespace, and workload",
		},
		[]string{"type", "namespace", "workload"},
	)

	IncidentsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "auto_agent_incidents_total",
			Help: "Count of incidents detected by reason, namespace, and workload",
		},
		[]string{"reason", "namespace", "workload"},
	)

	DedupSkippedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "auto_agent_dedup_skipped_total",
			Help: "Events skipped by deduplication",
		},
		[]string{"reason"},
	)

	RateLimitedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "auto_agent_rate_limited_total",
			Help: "Actions blocked by the global rate limiter",
		},
	)

	LLMRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "auto_agent_llm_requests_total",
			Help: "LLM diagnosis requests by status",
		},
		[]string{"status"},
	)

	HandlerErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "auto_agent_handler_errors_total",
			Help: "Errors encountered in event handlers",
		},
		[]string{"handler", "error_type"},
	)

	ScalingDecisionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "auto_agent_scaling_decisions_total",
			Help: "Scaling decisions made",
		},
		[]string{"direction", "namespace", "deployment"},
	)

	AnomaliesDetectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "auto_agent_anomalies_detected_total",
			Help: "Anomalies detected via CRD policies",
		},
		[]string{"policy", "namespace", "rule"},
	)

	InfoGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "auto_agent_info",
			Help: "Auto-agent build and config info",
		},
		[]string{"version", "mode"},
	)
)

func init() {
	prometheus.MustRegister(
		ActionsTotal,
		IncidentsTotal,
		DedupSkippedTotal,
		RateLimitedTotal,
		LLMRequestsTotal,
		HandlerErrorsTotal,
		ScalingDecisionsTotal,
		AnomaliesDetectedTotal,
		InfoGauge,
	)
}
