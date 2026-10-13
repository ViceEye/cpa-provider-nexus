package quotaactivation

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestParseClaudeAuthMaterial(t *testing.T) {
	for _, test := range []struct {
		name    string
		raw     string
		token   string
		wantErr bool
	}{
		{"oauth", `{"type":"claude","access_token":"fixture-access"}`, "fixture-access", false},
		{"nested oauth", `{"type":"claude","api_key":"fixture-key","token_data":{"access_token":"fixture-access"}}`, "fixture-access", false},
		{"wrong provider", `{"type":"codex","access_token":"fixture-access"}`, "", true},
		{"missing provider", `{"access_token":"fixture-access"}`, "", true},
		{"api key only", `{"type":"claude","api_key":"fixture-key"}`, "", true},
		{"refresh token only", `{"type":"claude","refresh_token":"fixture-refresh"}`, "", true},
		{"disabled", `{"type":"claude","access_token":"fixture-access","disabled":true}`, "", true},
		{"empty token", `{"type":"claude","access_token":" "}`, "", true},
		{"malformed", `{`, "", true},
		{"null", `null`, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			material, err := ParseClaudeAuthMaterial([]byte(test.raw))
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, test.wantErr)
			}
			if err == nil && material.AccessToken != test.token {
				t.Fatal("unexpected OAuth token")
			}
		})
	}
}

func TestBuildClaudeProtocolUsesNativeOAuthMessages(t *testing.T) {
	request, err := BuildClaudeProtocol(AuthMaterial{AccessToken: "fixture-access"}, " nexus/claude/claude-sonnet-4-5 ", "ping")
	if err != nil {
		t.Fatal(err)
	}
	if request.Method != http.MethodPost || request.URL != ClaudeActivationURL {
		t.Fatal("unexpected upstream request")
	}
	for header, expected := range map[string]string{
		"Authorization":     "Bearer fixture-access",
		"Anthropic-Version": "2023-06-01",
		"Anthropic-Beta":    "claude-code-20250219,oauth-2025-04-20",
		"Content-Type":      "application/json",
		"Accept":            "application/json",
		"X-App":             "cli",
	} {
		if request.Headers.Get(header) != expected {
			t.Errorf("unexpected %s header", header)
		}
	}
	if !strings.HasPrefix(request.Headers.Get("User-Agent"), "claude-cli/") || request.Headers.Get("X-Api-Key") != "" {
		t.Fatal("request did not use OAuth CLI headers")
	}
	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "claude-sonnet-4-5" || body.MaxTokens != 1 || body.Stream ||
		body.System != "You are Claude Code, Anthropic's official CLI for Claude." ||
		len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != "ping" {
		t.Fatalf("unexpected Messages payload: %s", request.Body)
	}
}

func TestBuildClaudeProtocolRejectsMissingInputs(t *testing.T) {
	if _, err := BuildClaudeProtocol(AuthMaterial{}, "claude-sonnet-4-5", "ping"); err == nil {
		t.Fatal("accepted missing access token")
	}
	if _, err := BuildClaudeProtocol(AuthMaterial{AccessToken: "fixture-access"}, "claude/", "ping"); err == nil {
		t.Fatal("accepted missing model")
	}
	request, err := BuildClaudeProtocol(AuthMaterial{AccessToken: "fixture-access"}, "claude-sonnet-4-5", "")
	if err != nil || !strings.Contains(string(request.Body), "quota activation ping") {
		t.Fatal("missing default activation prompt")
	}
}

func TestEvaluateClaudeActivationSuccess(t *testing.T) {
	valid := `{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"text","text":"Hi"}],"stop_reason":"max_tokens","usage":{"input_tokens":12,"output_tokens":1}}`
	for _, test := range []struct {
		name   string
		status int
		body   string
		wantOK bool
	}{
		{"max tokens", 200, valid, true},
		{"end turn", 200, strings.Replace(valid, "max_tokens", "end_turn", 1), true},
		{"unauthorized", 401, valid, false},
		{"quota exceeded", 429, `{"error":{"message":"fixture secret"}}`, false},
		{"server error", 500, valid, false},
		{"empty", 200, "", false},
		{"malformed", 200, `{`, false},
		{"unstructured", 200, `{"ok":true}`, false},
		{"business error", 200, `{"type":"error","error":{"message":"fixture secret"}}`, false},
		{"error with message", 200, strings.Replace(valid, `"type":"message"`, `"type":"message","error":{"message":"fixture secret"}`, 1), false},
		{"missing id", 200, strings.Replace(valid, `"msg_fixture"`, `""`, 1), false},
		{"wrong role", 200, strings.Replace(valid, `"assistant"`, `"user"`, 1), false},
		{"no output", 200, strings.Replace(valid, `"text":"Hi"`, `"text":""`, 1), false},
		{"missing usage", 200, strings.Replace(valid, `"usage"`, `"other"`, 1), false},
		{"no generated tokens", 200, strings.Replace(valid, `"output_tokens":1`, `"output_tokens":0`, 1), false},
		{"unfinished", 200, strings.Replace(valid, `"max_tokens"`, `null`, 1), false},
		{"unexpected stop", 200, strings.Replace(valid, `"max_tokens"`, `"tool_use"`, 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ok, message := EvaluateClaudeActivationSuccess(test.status, []byte(test.body))
			if ok != test.wantOK {
				t.Fatalf("ok = %v, message = %q", ok, message)
			}
			if !ok && (message == "" || strings.Contains(message, "fixture secret")) {
				t.Fatal("failure must report a sanitized error")
			}
		})
	}
}
