package obs

import "github.com/prometheus/client_golang/prometheus"

var (
    ActionsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "auto_agent_actions_total", Help: "Count of actions by type"},
        []string{"type","namespace","workload"},
    )
    IncidentsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "auto_agent_incidents_total", Help: "Incidents detected"},
        []string{"reason","namespace","workload"},
    )
)

func init() {
    prometheus.MustRegister(ActionsTotal, IncidentsTotal)
}
