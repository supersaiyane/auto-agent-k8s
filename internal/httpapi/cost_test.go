package httpapi

import (
	"testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
)

// PLAN-002 9.3: the Cost tab source and prices come from config, one case
// per source.
func TestNewCostConfig_Sources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     config.Cost
		source string
		cpu    float64
	}{
		{"default", config.Cost{}, "default", 0.05},
		{"manual", config.Cost{CPUPerHour: "0.07", MemPerGiBHour: "0.01"}, "manual", 0.07},
		{"manual with bad number keeps default price", config.Cost{CPUPerHour: "cheap"}, "manual", 0.05},
		{"kubecost wins", config.Cost{KubecostURL: "http://kc", OpenCostURL: "http://oc", CPUPerHour: "0.07"}, "kubecost", 0.07},
		{"opencost", config.Cost{OpenCostURL: "http://oc"}, "opencost", 0.05},
		{"instance prices", config.Cost{InstancePrices: "t3.medium=0.0416, m5.xlarge=0.192,bad"}, "instance-type", 0.05},
		{"unparseable instance prices are ignored", config.Cost{InstancePrices: "t3.medium=x"}, "default", 0.05},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCostConfig(tc.in)
			if c.Source != tc.source || c.CPUPerHour != tc.cpu {
				t.Fatalf("source %q cpu %v; want %q %v", c.Source, c.CPUPerHour, tc.source, tc.cpu)
			}
			if c.Currency != "USD" || c.HoursPerMonth != 730 {
				t.Fatalf("defaults: currency %q hours %v", c.Currency, c.HoursPerMonth)
			}
		})
	}
	c := newCostConfig(config.Cost{InstancePrices: "t3.medium=0.0416,m5.xlarge=0.192", Currency: "EUR"})
	if c.InstancePrices["m5.xlarge"] != 0.192 || len(c.InstancePrices) != 2 || c.Currency != "EUR" {
		t.Fatalf("instance prices or currency: %+v", c)
	}
	if rate, src := c.nodeHourlyRate("m5.xlarge", 4, 16); rate != 0.192 || src == "" {
		t.Fatalf("instance-type node rate: %v %q", rate, src)
	}
}
