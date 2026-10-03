package workbuddy

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDecodeCredentialAndNeedsRefresh(t *testing.T) {
	// OAuth Credential with expired token
	expiredOAuth := `{
		"type": "nexus",
		"kind": "workbuddy",
		"region": "cn",
		"access_token": "expired_token",
		"refresh_token": "refresh_123",
		"expires_at": "2020-01-01T00:00:00Z"
	}`
	cred, err := decodeCredential([]byte(expiredOAuth))
	if err != nil {
		t.Fatalf("decodeCredential failed: %v", err)
	}
	if cred.Kind != TypeMarker {
		t.Fatalf("expected kind %q, got %q", TypeMarker, cred.Kind)
	}
	if cred.Region != RegionCN {
		t.Fatalf("expected region %q, got %q", RegionCN, cred.Region)
	}
	if !credentialNeedsRefresh(cred) {
		t.Fatal("expected expired token to need refresh")
	}

	// OAuth Credential with valid token
	validOAuth := `{
		"type": "nexus",
		"kind": "workbuddy",
		"region": "intl",
		"access_token": "valid_token",
		"refresh_token": "refresh_123",
		"expires_at": "` + time.Now().Add(2*time.Hour).Format(time.RFC3339) + `"
	}`
	credValid, err := decodeCredential([]byte(validOAuth))
	if err != nil {
		t.Fatalf("decodeCredential failed: %v", err)
	}
	if credValid.Region != RegionIntl {
		t.Fatalf("expected region %q, got %q", RegionIntl, credValid.Region)
	}
	if credentialNeedsRefresh(credValid) {
		t.Fatal("expected valid token to not need refresh")
	}

	// API Key Credential (no refresh token)
	apiKeyCred := `{
		"type": "nexus",
		"kind": "workbuddy",
		"api_key": "sk-123456"
	}`
	credAPI, err := decodeCredential([]byte(apiKeyCred))
	if err != nil {
		t.Fatalf("decodeCredential failed: %v", err)
	}
	if credentialNeedsRefresh(credAPI) {
		t.Fatal("expected pure api_key credential to not need refresh")
	}

	// Stable ID test
	id1 := StableCredentialID([]byte(validOAuth))
	id2 := StableCredentialID([]byte(validOAuth))
	if id1 == "" || id1 != id2 {
		t.Fatalf("expected stable credential ID to match, got %q vs %q", id1, id2)
	}
}

func TestTransformPayloadCN(t *testing.T) {
	// 1. Agent prompt detection and neutralization
	req := map[string]any{
		"model": "nexus/glm-5.3",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are Claude Code, Anthropic's official CLI tool."},
			map[string]any{"role": "user", "content": "hello world"},
		},
		"reasoning_effort": "high",
		"stream":           false,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	transformedBytes, err := transformPayload(raw, "glm-5.3", RegionCN)
	if err != nil {
		t.Fatalf("transformPayload failed: %v", err)
	}

	var transformed map[string]any
	if err := json.Unmarshal(transformedBytes, &transformed); err != nil {
		t.Fatalf("unmarshal transformed failed: %v", err)
	}

	// Stream must be forced to true
	if stream, _ := transformed["stream"].(bool); !stream {
		t.Fatal("expected stream to be true")
	}

	// Target model should be updated
	if model, _ := transformed["model"].(string); model != "glm-5.3" {
		t.Fatalf("expected model glm-5.3, got %q", model)
	}

	// Reasoning summary should be auto
	if sum, _ := transformed["reasoning_summary"].(string); sum != "auto" {
		t.Fatalf("expected reasoning_summary auto, got %q", sum)
	}

	// System prompt should be neutralized
	messages, ok := transformed["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %v", transformed["messages"])
	}
	sysMsg, _ := messages[0].(map[string]any)
	sysContent, _ := sysMsg["content"].(string)
	if sysContent != neutralPrompt {
		t.Fatalf("expected system prompt to be neutralized, got %q", sysContent)
	}

	// 2. Legitimate user system prompt should NOT be neutralized
	legitReq := map[string]any{
		"model": "glm-5.3",
		"messages": []any{
			map[string]any{"role": "system", "content": "Translate English to French."},
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	legitRaw, _ := json.Marshal(legitReq)
	legitTransformedBytes, err := transformPayload(legitRaw, "glm-5.3", RegionCN)
	if err != nil {
		t.Fatalf("transformPayload failed: %v", err)
	}
	var legitTransformed map[string]any
	_ = json.Unmarshal(legitTransformedBytes, &legitTransformed)
	legitMsgs := legitTransformed["messages"].([]any)
	legitSys := legitMsgs[0].(map[string]any)["content"].(string)
	if legitSys != "Translate English to French." {
		t.Fatalf("expected legitimate system prompt to be preserved, got %q", legitSys)
	}
}

func TestTransformPayloadIntl(t *testing.T) {
	req := map[string]any{
		"model": "nexus/glm-5.2",
		"messages": []any{
			map[string]any{"role": "system", "content": "Custom system instructions."},
			map[string]any{"role": "user", "content": "Explain quantum computing."},
		},
		"reasoning_effort": "none",
	}
	raw, _ := json.Marshal(req)

	transformedBytes, err := transformPayload(raw, "glm-5.2", RegionIntl)
	if err != nil {
		t.Fatalf("transformPayload failed: %v", err)
	}

	var transformed map[string]any
	if err := json.Unmarshal(transformedBytes, &transformed); err != nil {
		t.Fatalf("unmarshal transformed failed: %v", err)
	}

	// reasoning_effort "none" should be deleted
	if _, ok := transformed["reasoning_effort"]; ok {
		t.Fatal("expected reasoning_effort to be removed when none")
	}

	messages := transformed["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}

	// First message should be CodeBuddy Code system prompt
	firstMsg := messages[0].(map[string]any)
	if firstMsg["role"] != "system" || firstMsg["content"] != "You are CodeBuddy Code." {
		t.Fatalf("expected leading system prompt for Intl, got %v", firstMsg)
	}

	// User message should have content wrapped as typed block
	userMsg := messages[1].(map[string]any)
	contentList, ok := userMsg["content"].([]any)
	if !ok || len(contentList) != 1 {
		t.Fatalf("expected typed blocks for user content, got %v", userMsg["content"])
	}
	block := contentList[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "Explain quantum computing." {
		t.Fatalf("unexpected content block: %v", block)
	}
}

func TestAggregateSSEChunks(t *testing.T) {
	mockSSE := `data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":" world!"}}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"reasoning_content":"Thinking deeply..."}}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]
`

	aggregatedBytes, err := aggregateSSEChunks([]byte(mockSSE), "glm-5.3")
	if err != nil {
		t.Fatalf("aggregateSSEChunks failed: %v", err)
	}

	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(aggregatedBytes, &resp); err != nil {
		t.Fatalf("unmarshal aggregated response failed: %v", err)
	}

	if resp.ID != "chatcmpl-1" {
		t.Fatalf("expected ID chatcmpl-1, got %q", resp.ID)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello world!" {
		t.Fatalf("expected content 'Hello world!', got %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].Message.ReasoningContent != "Thinking deeply..." {
		t.Fatalf("expected reasoning content 'Thinking deeply...', got %q", resp.Choices[0].Message.ReasoningContent)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("expected finish_reason 'stop', got %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Fatalf("expected total tokens 15, got %d", resp.Usage.TotalTokens)
	}
}

func TestModelsForAuth(t *testing.T) {
	cnStorage := `{"type":"nexus","kind":"workbuddy","region":"cn","access_token":"token"}`
	cnReq, _ := json.Marshal(map[string]any{"StorageJSON": []byte(cnStorage)})
	cnRespBytes, err := ModelsForAuth(cnReq)
	if err != nil {
		t.Fatalf("ModelsForAuth CN failed: %v", err)
	}

	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Models []modelInfo `json:"Models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(cnRespBytes, &env); err != nil || !env.OK {
		t.Fatalf("invalid envelope: %v", err)
	}

	foundGLM := false
	for _, m := range env.Result.Models {
		if m.ID == "nexus/glm-5.3" {
			foundGLM = true
			break
		}
	}
	if !foundGLM {
		t.Fatal("expected nexus/glm-5.3 in CN models list")
	}

	// Intl models
	intlStorage := `{"type":"nexus","kind":"workbuddy","region":"intl","access_token":"token"}`
	intlReq, _ := json.Marshal(map[string]any{"StorageJSON": []byte(intlStorage)})
	intlRespBytes, err := ModelsForAuth(intlReq)
	if err != nil {
		t.Fatalf("ModelsForAuth Intl failed: %v", err)
	}
	_ = json.Unmarshal(intlRespBytes, &env)
	foundIntlModel := false
	for _, m := range env.Result.Models {
		if m.ID == "nexus/deepseek-v4.1-flash" {
			foundIntlModel = true
			break
		}
	}
	if !foundIntlModel {
		t.Fatal("expected nexus/deepseek-v4.1-flash in Intl models list")
	}
}
