package projectstack

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

type serviceTunnelStub struct {
	local  string
	remote string
	closed string
}

func (s *serviceTunnelStub) Ensure(_ context.Context, _ string, remote string) (string, error) {
	s.remote = remote
	return s.local, nil
}

func (s *serviceTunnelStub) Close(key string) { s.closed = key }

// TestInspectReadsThePrivateServiceAddress covers the delimited inspect
// output. Health and the private container address are both allowed to be empty, so the
// fields are separated explicitly; parsing them positionally by whitespace would
// silently read a port as a health status.
func TestInspectReadsThePrivateServiceAddress(t *testing.T) {
	cases := []struct {
		name        string
		stdout      string
		wantState   State
		wantHealth  string
		wantAddress string
	}{
		{
			name:        "running with a private container address",
			stdout:      "running|healthy|10.89.0.5\n",
			wantState:   StateRunning,
			wantHealth:  "healthy",
			wantAddress: "10.89.0.5:8080",
		},
		{
			name:       "running with no container address",
			stdout:     "running|healthy|\n",
			wantState:  StateRunning,
			wantHealth: "healthy",
		},
		{
			name:        "no healthcheck but a private address",
			stdout:      "running||10.89.0.5\n",
			wantState:   StateStarting,
			wantAddress: "10.89.0.5:8080",
		},
		{
			name:      "exited container publishes nothing",
			stdout:    "exited||\n",
			wantState: StateStopped,
		},
		{
			// A container created by an earlier build reports the older
			// whitespace-separated form. It must still be readable rather than
			// unparseable, because the fix is a restart the operator has not made
			// yet.
			name:       "older whitespace form still parses",
			stdout:     "running healthy\n",
			wantState:  StateRunning,
			wantHealth: "healthy",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runner := &fakeRunner{responses: []fakeResponse{
				{match: "info", stdout: `{"host":{"os":"linux"}}`},
				{match: "inspect", stdout: testCase.stdout},
			}}
			manager := NewManagerWithRunner(runner)

			status, err := manager.Status(context.Background(), testProject("alpha-abcd1234", absPath("work", "alpha")))
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status.State != testCase.wantState {
				t.Errorf("state = %q, want %q", status.State, testCase.wantState)
			}
			if status.Health != testCase.wantHealth {
				t.Errorf("health = %q, want %q", status.Health, testCase.wantHealth)
			}
			if status.ServiceAddress != testCase.wantAddress {
				t.Errorf("service address = %q, want %q", status.ServiceAddress, testCase.wantAddress)
			}
		})
	}
}

func TestInspectExposesOnlyTheProcessOwnedLoopbackTunnel(t *testing.T) {
	runner := &fakeRunner{responses: []fakeResponse{
		{match: "info", stdout: `{"host":{"os":"linux"}}`},
		{match: "inspect", stdout: "running|healthy|10.89.0.5\n"},
	}}
	tunnel := &serviceTunnelStub{local: "127.0.0.1:53124"}
	manager := NewManagerWithRunner(runner)
	manager.tunnel = tunnel
	status, err := manager.Status(t.Context(), testProject("alpha-abcd1234", absPath("work", "alpha")))
	if err != nil {
		t.Fatal(err)
	}
	if status.ServiceAddress != tunnel.local || tunnel.remote != "10.89.0.5:8080" {
		t.Fatalf("status=%+v tunnel=%+v", status, tunnel)
	}
}

// TestRuntimePlanDoesNotPublishAProjectPort keeps every service inside the
// managed machine; Windows access belongs exclusively to the authenticated
// process-local tunnel.
func TestRuntimePlanDoesNotPublishAProjectPort(t *testing.T) {
	plan, err := BuildRuntimePlan(testProject("alpha-abcd1234", absPath("work", "alpha")), testBudget)
	if err != nil {
		t.Fatal(err)
	}
	document := strings.Join(plan.Arguments(), " ")

	if strings.Contains(document, "--publish") || strings.Contains(document, strconv.Itoa(ServicePort)+":"+strconv.Itoa(ServicePort)) {
		t.Errorf("runtime plan publishes the project service outside the container:\n%s", document)
	}
}

func TestRuntimePlanRenderingStaysReproducibleWithoutPublishedPorts(t *testing.T) {
	project := testProject("alpha-abcd1234", absPath("work", "alpha"))
	first, err := BuildRuntimePlan(project, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildRuntimePlan(project, testBudget)
	if err != nil {
		t.Fatal(err)
	}
	firstBody, err := RenderRuntimePlan(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := RenderRuntimePlan(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBody) != string(secondBody) {
		t.Error("rendering the same project twice produced different bytes")
	}
}
