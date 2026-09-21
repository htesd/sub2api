package openai

import "testing"

func TestMigrationResponseEvidence(t *testing.T) {
	for _, tc := range []struct {
		payload, event string
		ok             bool
	}{
		{`{"response":{"status":"completed"}}`, "response.completed", true},
		{`{"response":{"status":"failed"}}`, "response.completed", false},
		{`{"response":{"status":"in_progress"}}`, "response.done", false},
		{`{"response":{"status":"completed"},"status":"failed"}`, "response.completed", false},
		{`not-json`, "response.completed", false},
	} {
		if got := CompletedResponseEvidence([]byte(tc.payload), tc.event); got != tc.ok {
			t.Fatalf("payload=%s got=%v", tc.payload, got)
		}
	}
	if !ConflictingResponseModels([]byte(`{"response":{"model":"gpt-6-astra"},"model":"gpt-5.6-luna"}`)) {
		t.Fatal("conflict missed")
	}
}
