package adapter

import "testing"

func TestRPMOstreeStatusHeld(t *testing.T) {
	tests := []struct {
		name string
		body string
		held bool
	}{
		{"idle", `{"deployments":[{"booted":true}],"transaction":null}`, false},
		{"absent", `{"deployments":[]}`, false},
		{"upgrade", `{"transaction":{"kind":"upgrade"}}`, true},
		{"deploy", `{"transaction":{}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, err := ParseRPMOstreeStatus([]byte(tt.body))
			if err != nil {
				t.Fatalf("ParseRPMOstreeStatus: %v", err)
			}
			if status.Held() != tt.held {
				t.Errorf("Held() = %v, want %v", status.Held(), tt.held)
			}
		})
	}
}

func TestRPMOstreeStatusKeepsDeployments(t *testing.T) {
	status, err := ParseRPMOstreeStatus([]byte(`{"deployments":[{"booted":true,"staged":true,"version":"44.1","checksum":"abcdef"}]}`))
	if err != nil {
		t.Fatalf("ParseRPMOstreeStatus: %v", err)
	}
	if len(status.Deployments) != 1 {
		t.Fatalf("got %d deployments, want 1", len(status.Deployments))
	}
	got := status.Deployments[0]
	if !got.Booted || !got.Staged || got.Version != "44.1" || got.Checksum != "abcdef" {
		t.Errorf("deployment %+v does not match the document", got)
	}
}

func TestParseRPMOstreeStatusRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseRPMOstreeStatus([]byte("{")); err == nil {
		t.Error("ParseRPMOstreeStatus accepted a torn document")
	}
}
