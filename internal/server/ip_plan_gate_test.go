package server

import (
	"strings"
	"testing"

	"pvekube/internal/ipplan"
)

func TestBlockingIPPlanErrorRejectsErrorsButAllowsWarnings(t *testing.T) {
	issues := []ipplan.Issue{
		{Field: "gateway", Severity: ipplan.SeverityWarn, Message: "gateway responded"},
	}
	if err := blockingIPPlanError(issues); err != nil {
		t.Fatalf("warning unexpectedly blocked preview/apply: %v", err)
	}

	issues = append(issues, ipplan.Issue{
		Field: "control_plane_endpoint_ip", Severity: ipplan.SeverityError, Message: "address is occupied",
	})
	err := blockingIPPlanError(issues)
	if err == nil {
		t.Fatal("error-severity IP issue did not block preview/apply")
	}
	for _, want := range []string{"IP plan is invalid", "control_plane_endpoint_ip", "address is occupied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("blocking error %q does not contain %q", err, want)
		}
	}
}
