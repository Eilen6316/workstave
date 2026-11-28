package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"workstave/internal/outcome"
	"workstave/internal/runner"
	"workstave/internal/store"
)

type labeledMessage struct {
	ID             string
	ConversationID string
	Body           string
	Expected       string
	Kind           string
	Key            string
	ValueContains  []string
}

type sampleMessage struct {
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	Body           string `json:"body"`
}

type extractionOutcome struct {
	MessageID   string `json:"message_id"`
	Disposition string `json:"disposition"`
	Kind        string `json:"kind"`
	Key         string `json:"key"`
	Value       string `json:"value"`
	Evidence    string `json:"evidence"`
}

type benchmarkAnswer struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}

type extractionEnvelope struct {
	Messages []extractionOutcome `json:"messages"`
	Answers  []benchmarkAnswer   `json:"answers"`
}

type answerEnvelope struct {
	Answers []benchmarkAnswer `json:"answers"`
}

type benchmarkRun struct {
	Text        string
	PromptChars int
	Error       error
}

var benchmarkSecret = regexp.MustCompile(`sk-[A-Za-z0-9*_-]{6,}`)

const extractionSchema = `{"type":"object","additionalProperties":false,"properties":{"messages":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"message_id":{"type":"string"},"disposition":{"type":"string","enum":["confirmed","candidate","clarify","none"]},"kind":{"type":"string"},"key":{"type":"string"},"value":{"type":"string"},"evidence":{"type":"string"}},"required":["message_id","disposition","kind","key","value","evidence"]}},"answers":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"question_id":{"type":"string"},"answer":{"type":"string"}},"required":["question_id","answer"]}}},"required":["messages","answers"]}`

const extractionOnlySchema = `{"type":"object","additionalProperties":false,"properties":{"messages":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"message_id":{"type":"string"},"disposition":{"type":"string","enum":["confirmed","candidate","clarify","none"]},"kind":{"type":"string"},"key":{"type":"string"},"value":{"type":"string"},"evidence":{"type":"string"}},"required":["message_id","disposition","kind","key","value","evidence"]}}},"required":["messages"]}`

const answerOnlySchema = `{"type":"object","additionalProperties":false,"properties":{"answers":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"question_id":{"type":"string"},"answer":{"type":"string"}},"required":["question_id","answer"]}}},"required":["answers"]}`

var labeledSample = []labeledMessage{
	{ID: "M01", ConversationID: "A", Body: "认证服务必须使用 JWT。", Expected: "confirmed", Kind: "requirement", Key: "auth_method", ValueContains: []string{"jwt"}},
	{ID: "M02", ConversationID: "A", Body: "用户资料导出必须支持 CSV。", Expected: "confirmed", Kind: "requirement", Key: "export_format", ValueContains: []string{"csv"}},
	{ID: "M03", ConversationID: "A", Body: "JWT 会不会比 Cookie 更适合移动端？", Expected: "candidate_or_none"},
	{ID: "M04", ConversationID: "A", Body: "导出时是不是也应该支持 TSV？", Expected: "candidate_or_none"},
	{ID: "M05", ConversationID: "A", Body: "如果刷新令牌失败，用户可能需要重新登录。", Expected: "candidate_or_none"},
	{ID: "M06", ConversationID: "A", Body: "假设 CSV 导出遇到空字段，可以留空。", Expected: "candidate_or_none"},
	{ID: "M07", ConversationID: "A", Body: "访问令牌过期时间你按常见默认值定。", Expected: "confirmed", Kind: "decision", Key: "access_token_ttl", ValueContains: []string{"min", "minute", "hour", "30", "60"}},
	{ID: "M08", ConversationID: "A", Body: "导出分页大小你按常见默认值决定。", Expected: "confirmed", Kind: "decision", Key: "export_page_size", ValueContains: []string{"25", "50", "100"}},
	{ID: "M09", ConversationID: "A", Body: "取消 JWT，改为 Cookie Session。", Expected: "confirmed", Kind: "requirement", Key: "auth_method", ValueContains: []string{"cookie"}},
	{ID: "M10", ConversationID: "A", Body: "导出格式改成 TSV，不再使用 CSV。", Expected: "confirmed", Kind: "requirement", Key: "export_format", ValueContains: []string{"tsv"}},
	{ID: "M11", ConversationID: "B", Body: "接口最好快一点。", Expected: "clarify"},
	{ID: "M12", ConversationID: "B", Body: "把保留期放宽一些。", Expected: "clarify"},
	{ID: "M13", ConversationID: "A", Body: "我刚才说错了，认证应为 JWT，不是 Cookie。", Expected: "confirmed", Kind: "requirement", Key: "auth_method", ValueContains: []string{"jwt"}},
	{ID: "M14", ConversationID: "A", Body: "纠正导出要求，最终应是 CSV，不是 TSV。", Expected: "confirmed", Kind: "requirement", Key: "export_format", ValueContains: []string{"csv"}},
	{ID: "M15", ConversationID: "A", Body: "今天先到这里。", Expected: "none"},
	{ID: "M16", ConversationID: "B", Body: "收到，谢谢。", Expected: "none"},
}

// TestRealExtractionMatrix uses one frozen, labeled message set for both Harnesses. It compares
// detailed and concise single-call prompts, a separate extraction/answer pair, and raw transcript
// input against per-message JSON envelopes. Results are observations, not pass/fail product claims.
func TestRealExtractionMatrix(t *testing.T) {
	if os.Getenv("WORKSTAVE_REAL_HARNESS") != "1" {
		t.Skip("set WORKSTAVE_REAL_HARNESS=1 to run the billable extraction matrix")
	}

	fullInstructions := "You are extracting a chronological work record from two Conversations. Each input row has a stable message id and owner. For every row return exactly one classification. Confirm only explicit requirements, explicit changes or corrections, and decisions the user delegated to Main. A question or hypothesis may be candidate or none, never confirmed. An ambiguous request must be clarify. Chatter is none. Preserve corrections as new confirmed facts that supersede the earlier value. A delegated decision must choose a concrete ordinary default and cite the delegation as evidence. Every confirmed fact needs kind, key, value and an exact evidence quote from its source message. Do not treat quoted or hypothetical values as accepted decisions. Answer the two recall questions from the final confirmed facts. Return only the schema-shaped JSON."
	shortInstructions := "Classify each message as confirmed, candidate, clarify, or none. Confirm only explicit requirements/changes/corrections or user-delegated decisions. Questions and hypotheses cannot replace confirmed facts. Cite exact evidence. Then answer Q01 and Q02 from final confirmed facts. Return schema-shaped JSON only."
	extractionInstructions := "Classify each message as confirmed, candidate, clarify, or none. Confirm only explicit requirements/changes/corrections or user-delegated decisions. Questions and hypotheses cannot replace confirmed facts. Cite exact evidence. Return one classification per message in schema-shaped JSON. Do not answer questions."
	questions := "Recall questions from Conversation B:\nQ01: 我们最终选了什么认证方式？\nQ02: 资料导出格式最后是什么？"

	for _, harnessName := range []string{"claude-code", "codex"} {
		t.Run(harnessName, func(t *testing.T) {
			envelopedInput := renderSample(labeledSample, true)
			transcriptInput := renderSample(labeledSample, false)
			fullPrompt := fullInstructions + "\nMessages:\n" + envelopedInput + "\n" + questions
			shortEnvelopePrompt := shortInstructions + "\nMessages:\n" + envelopedInput + "\n" + questions
			shortTranscriptPrompt := shortInstructions + "\nMessages:\n" + transcriptInput + "\n" + questions
			if utf8.RuneCountInString(fullPrompt) <= utf8.RuneCountInString(shortEnvelopePrompt) {
				t.Fatal("the full prompt must be longer than the short prompt")
			}

			full := runExtractionCall(t, harnessName, fullPrompt, extractionSchema, "")
			short := runExtractionCall(t, harnessName, shortEnvelopePrompt, extractionSchema, "")
			transcript := runExtractionCall(t, harnessName, shortTranscriptPrompt, extractionSchema, "")

			extractPrompt := extractionInstructions + "\nMessages:\n" + envelopedInput
			extracted := runExtractionCall(t, harnessName, extractPrompt, extractionOnlySchema, "")
			twoCallAnswers := benchmarkRun{}
			var extractedEnvelope extractionEnvelope
			if extracted.Error == nil {
				if err := json.Unmarshal([]byte(stripJSONFence(extracted.Text)), &extractedEnvelope); err != nil {
					extracted.Error = fmt.Errorf("decode extraction response: %w", err)
				} else {
					facts, err := json.Marshal(confirmedFacts(extractedEnvelope.Messages))
					if err != nil {
						extracted.Error = fmt.Errorf("encode extracted facts: %w", err)
					} else {
						answerPrompt := "Answer the two questions using only these extracted confirmed facts. Do not infer missing facts.\nConfirmed facts:\n" + string(facts) + "\n" + questions
						twoCallAnswers = runExtractionCall(t, harnessName, answerPrompt, answerOnlySchema, "")
					}
				}
			} else {
				twoCallAnswers.Error = extracted.Error
			}

			for _, result := range []struct {
				name string
				run  benchmarkRun
			}{{"single-full-envelope", full}, {"single-short-envelope", short}, {"single-short-transcript", transcript}} {
				t.Logf("harness=%s condition=%s prompt_chars=%d result=%s", harnessName, result.name, result.run.PromptChars, summarizeFullResponse(result.run))
			}
			t.Logf("harness=%s condition=two-call-extract-and-answer total_prompt_chars=%d extraction_prompt_chars=%d reply_prompt_chars=%d extraction=%s answers=%s", harnessName, extracted.PromptChars+twoCallAnswers.PromptChars, extracted.PromptChars, twoCallAnswers.PromptChars, summarizeExtraction(extracted), summarizeAnswers(twoCallAnswers))
			if harnessName == "claude-code" {
				// Production-aligned arm: uses the real mainAppendPrompt and compilePrompt, matching
				// Claude Main's prompt-driven JSON output without an output schema flag.
				mainSnap := inputSnapshot{
					Context:        store.MainContext{WorkID: "bench-work", Revision: 0},
					ConversationID: "bench-conv",
					CurrentInput:   "认证服务必须使用 JWT，导出格式为 CSV。",
				}
				prodPrompt := compilePrompt(mainSnap)
				prod := runExtractionCall(t, harnessName, prodPrompt, "", mainAppendPrompt)
				var prodCompletion store.MainCompletion
				var prodDecodeErr error
				if prod.Error == nil {
					prodCompletion, prodDecodeErr = decodeMainReply(prod.Text)
				}
				confirmedJWT := false
				for _, ch := range prodCompletion.Changes {
					if ch.Status == "confirmed" && strings.Contains(strings.ToLower(ch.Value), "jwt") {
						confirmedJWT = true
					}
				}
				t.Logf("harness=%s condition=production-main-prompt-with-append json_valid=%t confirmed_jwt=%t identity_response=%t",
					harnessName,
					prod.Error == nil && prodDecodeErr == nil,
					confirmedJWT,
					prod.Error == nil && (strings.Contains(strings.ToLower(prod.Text), "i am claude code") || strings.Contains(strings.ToLower(prod.Text), "you are")),
				)
			}
		})
	}
}

func renderSample(messages []labeledMessage, enveloped bool) string {
	if enveloped {
		rows := make([]sampleMessage, 0, len(messages))
		for _, message := range messages {
			rows = append(rows, sampleMessage{MessageID: message.ID, ConversationID: message.ConversationID, Body: message.Body})
		}
		encoded, _ := json.MarshalIndent(rows, "", "  ")
		return string(encoded)
	}
	var b strings.Builder
	for _, message := range messages {
		fmt.Fprintf(&b, "%s (%s): %s\n", message.ID, message.ConversationID, message.Body)
	}
	return b.String()
}

func runExtractionCall(t *testing.T, harnessName, prompt, schema, appendSystemPrompt string) benchmarkRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	result := benchmarkRun{PromptChars: utf8.RuneCountInString(prompt)}
	switch harnessName {
	case "claude-code":
		h := runner.Claude{MaxBudget: "0.10", Model: "claude-haiku-4-5-20251001"}
		execution, err := h.Execute(ctx, runner.Request{Dir: dir, NativeID: h.NewNativeID(), Prompt: prompt, OutputSchema: schema, AppendSystemPrompt: appendSystemPrompt}, func(string) {})
		if err != nil {
			result.Error = err
		} else if execution.Outcome != outcome.Completed {
			result.Error = fmt.Errorf("Claude Run outcome %s", execution.Outcome)
		} else {
			result.Text = benchmarkSecret.ReplaceAllString(execution.Text, "sk-<redacted>")
		}
	case "codex":
		h := runner.Codex{}
		execution, err := h.Execute(ctx, runner.Request{Dir: dir, Prompt: prompt, OutputSchema: schema}, func(string) {})
		if err != nil {
			result.Error = err
			break
		}
		if execution.Outcome != outcome.Completed {
			result.Error = fmt.Errorf("Codex Run outcome %s", execution.Outcome)
			break
		}
		result.Text = benchmarkSecret.ReplaceAllString(execution.Text, "sk-<redacted>")
	}
	if result.Error != nil {
		result.Error = fmt.Errorf("%s: %w", harnessName, result.Error)
	}
	return result
}

func summarizeExtraction(run benchmarkRun) string {
	if run.Error != nil {
		return "error=" + benchmarkSecret.ReplaceAllString(run.Error.Error(), "sk-<redacted>")
	}
	var response extractionEnvelope
	if err := json.Unmarshal([]byte(stripJSONFence(run.Text)), &response); err != nil {
		return "decode_error=" + benchmarkSecret.ReplaceAllString(err.Error(), "sk-<redacted>")
	}
	return scoreExtraction(response.Messages)
}

func summarizeFullResponse(run benchmarkRun) string {
	if run.Error != nil {
		return "error=" + benchmarkSecret.ReplaceAllString(run.Error.Error(), "sk-<redacted>")
	}
	var response extractionEnvelope
	if err := json.Unmarshal([]byte(stripJSONFence(run.Text)), &response); err != nil {
		return "decode_error=" + benchmarkSecret.ReplaceAllString(err.Error(), "sk-<redacted>")
	}
	return scoreExtraction(response.Messages) + " " + scoreAnswerItems(response.Answers)
}

func summarizeAnswers(run benchmarkRun) string {
	if run.Error != nil {
		return "error=" + benchmarkSecret.ReplaceAllString(run.Error.Error(), "sk-<redacted>")
	}
	var response answerEnvelope
	if err := json.Unmarshal([]byte(stripJSONFence(run.Text)), &response); err != nil {
		return "decode_error=" + benchmarkSecret.ReplaceAllString(err.Error(), "sk-<redacted>")
	}
	return scoreAnswerItems(response.Answers)
}

func scoreAnswerItems(answers []benchmarkAnswer) string {
	correct := 0
	seen := make(map[string]bool, len(answers))
	for _, answer := range answers {
		if seen[answer.QuestionID] {
			continue
		}
		seen[answer.QuestionID] = true
		if answer.QuestionID == "Q01" && strings.Contains(strings.ToLower(answer.Answer), "jwt") {
			correct++
		}
		if answer.QuestionID == "Q02" && strings.Contains(strings.ToLower(answer.Answer), "csv") {
			correct++
		}
	}
	return fmt.Sprintf("recall_correct=%d/2 answers=%v", correct, answers)
}

func scoreExtraction(actual []extractionOutcome) string {
	byID := make(map[string]extractionOutcome, len(actual))
	for _, result := range actual {
		byID[result.MessageID] = result
	}
	correct, required, requiredCorrect, falseConfirmed := 0, 0, 0, 0
	for _, expected := range labeledSample {
		result, found := byID[expected.ID]
		if !found {
			if expected.Expected == "confirmed" {
				required++
			}
			continue
		}
		matches := false
		switch expected.Expected {
		case "confirmed":
			required++
			matches = result.Disposition == "confirmed" && result.Kind == expected.Kind && result.Key == expected.Key
			value := strings.ToLower(result.Value)
			valueMatched := false
			for _, acceptable := range expected.ValueContains {
				if strings.Contains(value, acceptable) {
					valueMatched = true
				}
			}
			matches = matches && valueMatched && result.Evidence != "" && strings.Contains(expected.Body, result.Evidence)
			if matches {
				requiredCorrect++
			}
		case "candidate_or_none":
			matches = result.Disposition == "candidate" || result.Disposition == "none"
		case "clarify", "none":
			matches = result.Disposition == expected.Expected
		}
		if matches {
			correct++
		}
		if expected.Expected != "confirmed" && result.Disposition == "confirmed" {
			falseConfirmed++
		}
	}
	return fmt.Sprintf("classification=%d/%d required_confirmations=%d/%d false_confirmations=%d", correct, len(labeledSample), requiredCorrect, required, falseConfirmed)
}

func confirmedFacts(results []extractionOutcome) []extractionOutcome {
	var facts []extractionOutcome
	for _, result := range results {
		if result.Disposition == "confirmed" {
			facts = append(facts, result)
		}
	}
	return facts
}

func stripJSONFence(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```json") && strings.HasSuffix(text, "```") {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "```json"), "```"))
	}
	return text
}
