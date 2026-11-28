package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLabeledSampleHasStableIdentitiesAndEnvelopePreservesMessages(t *testing.T) {
	seen := make(map[string]bool, len(labeledSample))
	for _, message := range labeledSample {
		if message.ID == "" || message.ConversationID == "" || seen[message.ID] {
			t.Fatalf("invalid or repeated labeled message identity: %+v", message)
		}
		seen[message.ID] = true
	}
	transcript := renderSample(labeledSample, false)
	encoded := renderSample(labeledSample, true)
	if transcript == encoded {
		t.Fatal("transcript and JSON envelope rendered the same representation")
	}
	var envelope []sampleMessage
	if err := json.Unmarshal([]byte(encoded), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope) != len(labeledSample) {
		t.Fatalf("envelope contains %d messages, want %d", len(envelope), len(labeledSample))
	}
	for i, message := range labeledSample {
		if envelope[i].MessageID != message.ID || envelope[i].ConversationID != message.ConversationID || envelope[i].Body != message.Body {
			t.Fatalf("envelope message %d = %+v, want original sample %+v", i, envelope[i], message)
		}
	}
}

func TestExtractionScoringCountsMissesAndFalseConfirmations(t *testing.T) {
	actual := []extractionOutcome{
		{MessageID: "M01", Disposition: "confirmed", Kind: "requirement", Key: "auth_method", Value: "JWT", Evidence: "使用 JWT"},
		{MessageID: "M02", Disposition: "confirmed", Kind: "requirement", Key: "export_format", Value: "CSV", Evidence: "支持 CSV"},
		{MessageID: "M03", Disposition: "confirmed", Kind: "requirement", Key: "auth_method", Value: "Cookie", Evidence: "Cookie"},
		{MessageID: "M11", Disposition: "clarify"},
		{MessageID: "M15", Disposition: "none"},
	}
	result := scoreExtraction(actual)
	if !strings.Contains(result, "required_confirmations=2/8") || !strings.Contains(result, "false_confirmations=1") {
		t.Fatalf("extraction score=%q, want explicit misses and one false confirmation", result)
	}
}
