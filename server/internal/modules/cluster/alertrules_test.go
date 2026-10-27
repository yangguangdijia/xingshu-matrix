package cluster

import "testing"

func TestValidateAlertRuleInput(t *testing.T) {
	base := alertRuleInput{
		Namespace: "monitoring",
		RuleGroup: "matrix-ui",
		Name:      "NodeHighCPU",
		Expr:      `sum(rate(node_cpu_seconds_total{mode!="idle"}[5m])) > 0.85`,
		For:       "1h30m",
		Severity:  "warning",
		Summary:   "Node CPU is high",
	}
	if err := validateAlertRuleInput(base); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}

	tests := []struct {
		name   string
		change func(*alertRuleInput)
	}{
		{name: "invalid namespace", change: func(in *alertRuleInput) { in.Namespace = "Monitoring" }},
		{name: "invalid alert name", change: func(in *alertRuleInput) { in.Name = "1NodeHighCPU" }},
		{name: "empty expression", change: func(in *alertRuleInput) { in.Expr = "  " }},
		{name: "unsupported duration", change: func(in *alertRuleInput) { in.For = "five minutes" }},
		{name: "unsupported severity", change: func(in *alertRuleInput) { in.Severity = "urgent" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := base
			test.change(&in)
			if err := validateAlertRuleInput(in); err == nil {
				t.Fatal("invalid rule accepted")
			}
		})
	}
}
