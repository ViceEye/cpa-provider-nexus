package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormalizeQuotaTriggerScheduleRequiresModel(t *testing.T) {
	_, err := normalizeQuotaTriggerSchedule(quotaTriggerSchedule{
		Provider:  "antigravity",
		AuthIndex: "auth-1",
		Time:      "08:30",
		Timezone:  "Asia/Shanghai",
	})
	if err == nil || err.Error() != "缺少模型名称" {
		t.Fatalf("err = %v", err)
	}
}

func TestNormalizeQuotaTriggerModelRemovesProviderPrefix(t *testing.T) {
	for input, expected := range map[string]string{
		"nexus/gpt-5.4-mini":       "gpt-5.4-mini",
		"codex/gpt-5.4-mini":       "gpt-5.4-mini",
		"antigravity/gemini-flash": "gemini-flash",
		"claude/claude-sonnet-4-5": "claude-sonnet-4-5",
	} {
		if actual := normalizeQuotaTriggerModel(input); actual != expected {
			t.Errorf("normalizeQuotaTriggerModel(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func TestNormalizeQuotaTriggerScheduleAcceptsClaude(t *testing.T) {
	schedule, err := normalizeQuotaTriggerSchedule(quotaTriggerSchedule{
		Provider: "Claude", AuthIndex: "auth-claude", Model: "claude/claude-sonnet-4-5",
		Time: "08:30", Timezone: "Asia/Shanghai", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if schedule.Provider != "claude" || schedule.Model != "claude-sonnet-4-5" || schedule.ID == "" {
		t.Fatalf("schedule = %#v", schedule)
	}
}

func TestExecuteQuotaTriggerClaudeUsesSelectedCredential(t *testing.T) {
	originalCall, originalHTTP := callHostCall, hostHTTPDoCall
	t.Cleanup(func() { callHostCall, hostHTTPDoCall = originalCall, originalHTTP })
	callHostCall = func(method string, payload any) (json.RawMessage, error) {
		if payload.(map[string]any)["auth_index"] != "auth-claude" {
			t.Fatal("request selected another credential")
		}
		switch method {
		case "host.auth.get":
			result, _ := json.Marshal(hostAuthGetResponse{
				AuthIndex: "auth-claude", Name: "claude.json",
				JSON: json.RawMessage(`{"type":"claude","access_token":"fixture-access"}`),
			})
			return result, nil
		case "host.auth.get_runtime":
			return json.RawMessage(`{"auth":{"auth_index":"auth-claude","provider":"claude","disabled":false}}`), nil
		default:
			t.Fatalf("unexpected callback: %s", method)
			return nil, nil
		}
	}
	calls := 0
	hostHTTPDoCall = func(req hostHTTPRequest) (hostHTTPResponse, error) {
		calls++
		if req.HostCallbackID != "callback-claude" || req.Method != http.MethodPost ||
			req.URL != "https://api.anthropic.com/v1/messages?beta=true" ||
			req.Headers.Get("Authorization") != "Bearer fixture-access" {
			t.Fatal("unexpected host HTTP request")
		}
		if !strings.Contains(string(req.Body), `"model":"claude-sonnet-4-5"`) || !strings.Contains(string(req.Body), `"max_tokens":1`) {
			t.Fatalf("unexpected request body: %s", req.Body)
		}
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"id":"msg_fixture","type":"message","role":"assistant","content":[{"type":"text","text":"Hi"}],"stop_reason":"max_tokens","usage":{"input_tokens":12,"output_tokens":1}}`)}, nil
	}
	if err := executeQuotaTrigger(quotaTriggerSchedule{
		Provider: "claude", AuthIndex: "auth-claude", Model: "claude/claude-sonnet-4-5",
	}, "callback-claude"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
}

func TestExecuteQuotaTriggerClaudeFailsClosed(t *testing.T) {
	originalCall, originalHTTP := callHostCall, hostHTTPDoCall
	t.Cleanup(func() { callHostCall, hostHTTPDoCall = originalCall, originalHTTP })
	validAuth := `{"type":"claude","access_token":"fixture-access"}`
	validRuntime := `{"auth":{"auth_index":"auth-claude","provider":"claude","disabled":false}}`
	for _, test := range []struct {
		name       string
		authIndex  string
		auth       string
		runtime    string
		runtimeErr bool
	}{
		{"disabled runtime", "auth-claude", validAuth, strings.Replace(validRuntime, "false", "true", 1), false},
		{"disabled file", "auth-claude", `{"type":"claude","access_token":"fixture-access","disabled":true}`, validRuntime, false},
		{"wrong auth index", "other-auth", validAuth, validRuntime, false},
		{"wrong runtime index", "auth-claude", validAuth, strings.Replace(validRuntime, "auth-claude", "other-auth", 1), false},
		{"wrong runtime provider", "auth-claude", validAuth, strings.Replace(validRuntime, `"claude"`, `"codex"`, 1), false},
		{"wrong file provider", "auth-claude", `{"type":"codex","access_token":"fixture-access"}`, validRuntime, false},
		{"api key", "auth-claude", `{"type":"claude","api_key":"fixture-key"}`, validRuntime, false},
		{"runtime unavailable", "auth-claude", validAuth, validRuntime, true},
		{"runtime malformed", "auth-claude", validAuth, `{`, false},
		{"runtime missing", "auth-claude", validAuth, `{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			callHostCall = func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case "host.auth.get":
					result, _ := json.Marshal(hostAuthGetResponse{AuthIndex: test.authIndex, JSON: json.RawMessage(test.auth)})
					return result, nil
				case "host.auth.get_runtime":
					if test.runtimeErr {
						return nil, errors.New("fixture callback unavailable")
					}
					return json.RawMessage(test.runtime), nil
				default:
					t.Fatalf("unexpected callback: %s", method)
					return nil, nil
				}
			}
			hostHTTPDoCall = func(hostHTTPRequest) (hostHTTPResponse, error) {
				t.Fatal("invalid credential must not send an upstream request")
				return hostHTTPResponse{}, nil
			}
			if err := executeQuotaTrigger(quotaTriggerSchedule{
				Provider: "claude", AuthIndex: "auth-claude", Model: "claude-sonnet-4-5",
			}, ""); err == nil {
				t.Fatal("expected credential validation failure")
			}
		})
	}
}

func TestExecuteQuotaTriggerClaudeReportsUpstreamFailures(t *testing.T) {
	originalCall, originalHTTP := callHostCall, hostHTTPDoCall
	t.Cleanup(func() { callHostCall, hostHTTPDoCall = originalCall, originalHTTP })
	callHostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.get":
			result, _ := json.Marshal(hostAuthGetResponse{
				AuthIndex: "auth-claude",
				JSON:      json.RawMessage(`{"type":"claude","access_token":"fixture-access"}`),
			})
			return result, nil
		case "host.auth.get_runtime":
			return json.RawMessage(`{"auth":{"auth_index":"auth-claude","provider":"claude"}}`), nil
		default:
			t.Fatalf("unexpected callback: %s", method)
			return nil, nil
		}
	}
	for _, test := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{"rate limited", 429, `{"error":{"message":"fixture secret"}}`, nil},
		{"unauthorized", 401, `{"error":{"message":"fixture secret"}}`, nil},
		{"unstructured success", 200, `{"ok":true}`, nil},
		{"business error", 200, `{"type":"error","error":{"message":"fixture secret"}}`, nil},
		{"network failure", 0, "", errors.New("fixture secret")},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			hostHTTPDoCall = func(hostHTTPRequest) (hostHTTPResponse, error) {
				calls++
				return hostHTTPResponse{StatusCode: test.status, Body: []byte(test.body)}, test.err
			}
			err := executeQuotaTrigger(quotaTriggerSchedule{
				Provider: "claude", AuthIndex: "auth-claude", Model: "claude-sonnet-4-5",
			}, "")
			if err == nil || strings.Contains(err.Error(), "fixture secret") {
				t.Fatalf("expected sanitized failure, got %v", err)
			}
			if calls != 1 {
				t.Fatalf("upstream calls = %d, want 1 without credential fallback", calls)
			}
		})
	}
}

func TestQuotaTriggerDueAtUsesConfiguredTimezone(t *testing.T) {
	schedule := quotaTriggerSchedule{Provider: "codex", AuthIndex: "auth-1", Time: "08:30", Timezone: "Asia/Shanghai", Model: "gpt-5.4-mini"}
	now := time.Date(2026, time.September, 4, 0, 30, 0, 0, time.UTC)
	due, dateKey, ok := quotaTriggerDueAt(schedule, now)
	if !ok || dateKey != "2026-09-04" || due.Hour() != 8 || due.Minute() != 30 {
		t.Fatalf("due = %v date = %q ok = %v", due, dateKey, ok)
	}
}

func TestExecuteQuotaTriggerCodexUsesSelectedCredential(t *testing.T) {
	originalCall := callHostCall
	originalHTTP := hostHTTPDoCall
	t.Cleanup(func() {
		callHostCall = originalCall
		hostHTTPDoCall = originalHTTP
	})

	storage := []byte(`{"type":"codex","access_token":"fixture-access","account_id":"account-1"}`)
	callHostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.get":
			result, _ := json.Marshal(hostAuthGetResponse{AuthIndex: "auth-1", Name: "codex.json", JSON: storage})
			return result, nil
		case "host.auth.get_runtime":
			return json.RawMessage(`{"auth":{"disabled":false}}`), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	}
	hostHTTPDoCall = func(req hostHTTPRequest) (hostHTTPResponse, error) {
		if req.Method != http.MethodPost || req.URL != "https://chatgpt.com/backend-api/codex/responses" {
			t.Fatalf("request = %#v", req)
		}
		if req.Headers.Get("Authorization") != "Bearer fixture-access" || req.Headers.Get("Chatgpt-Account-Id") != "account-1" {
			t.Fatalf("headers = %#v", req.Headers)
		}
		if !strings.Contains(string(req.Body), `"model":"gpt-5.4-mini"`) || !strings.Contains(string(req.Body), `"stream":true`) {
			t.Fatalf("body = %s", req.Body)
		}
		return hostHTTPResponse{StatusCode: http.StatusOK, Body: []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n")}, nil
	}

	errTrigger := executeQuotaTrigger(quotaTriggerSchedule{
		ID: "qt-test", Provider: "codex", AuthIndex: "auth-1", Model: "gpt-5.4-mini",
		Time: "08:30", Timezone: "UTC", Enabled: true,
	}, "callback-1")
	if errTrigger != nil {
		t.Fatal(errTrigger)
	}
}

func TestExecuteQuotaTriggerAntigravityUsesProjectAndModel(t *testing.T) {
	originalCall := callHostCall
	originalHTTP := hostHTTPDoCall
	t.Cleanup(func() {
		callHostCall = originalCall
		hostHTTPDoCall = originalHTTP
	})

	storage := []byte(`{"type":"antigravity","access_token":"fixture-access","project_id":"project-1"}`)
	callHostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.get":
			result, _ := json.Marshal(hostAuthGetResponse{AuthIndex: "auth-1", Name: "antigravity.json", JSON: storage})
			return result, nil
		case "host.auth.get_runtime":
			return json.RawMessage(`{"auth":{"disabled":false}}`), nil
		default:
			t.Fatalf("unexpected host callback %q", method)
			return nil, nil
		}
	}
	hostHTTPDoCall = func(req hostHTTPRequest) (hostHTTPResponse, error) {
		if req.Method != http.MethodPost || req.URL != "https://daily-cloudcode-pa.googleapis.com/v1internal:generateContent" {
			t.Fatalf("request = %#v", req)
		}
		if req.Headers.Get("Authorization") != "Bearer fixture-access" {
			t.Fatalf("headers = %#v", req.Headers)
		}
		if !strings.Contains(string(req.Body), `"project":"projects/project-1"`) || !strings.Contains(string(req.Body), `"model":"gemini-3-flash"`) {
			t.Fatalf("body = %s", req.Body)
		}
		return hostHTTPResponse{StatusCode: http.StatusOK, Body: []byte(`{"response":{"candidates":[{}]}}`)}, nil
	}

	errTrigger := executeQuotaTrigger(quotaTriggerSchedule{
		ID: "qt-test", Provider: "antigravity", AuthIndex: "auth-1", Model: "gemini-3-flash",
		Time: "08:30", Timezone: "UTC", Enabled: true,
	}, "callback-1")
	if errTrigger != nil {
		t.Fatal(errTrigger)
	}
}
