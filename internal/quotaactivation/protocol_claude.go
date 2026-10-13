package quotaactivation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const ClaudeActivationURL = "https://api.anthropic.com/v1/messages?beta=true"

// ParseClaudeAuthMaterial only accepts Claude OAuth credentials, not API keys
// or another provider's token. API keys do not wake a subscription quota window.
func ParseClaudeAuthMaterial(raw []byte) (AuthMaterial, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return AuthMaterial{}, fmt.Errorf("Claude唤醒失败：凭证不是合法数据")
	}
	if !strings.EqualFold(firstStringField(root, "type"), "claude") {
		return AuthMaterial{}, fmt.Errorf("Claude唤醒失败：凭证提供方不匹配")
	}
	var disabled bool
	if json.Unmarshal(root["disabled"], &disabled) == nil && disabled {
		return AuthMaterial{}, fmt.Errorf("凭证已停用")
	}
	token := firstStringField(root, "access_token", "accessToken", "oauth_access_token", "oauthAccessToken")
	for _, key := range []string{"tokens", "token_data", "tokenData", "credentials", "auth", "oauth", "session"} {
		if token != "" {
			break
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(root[key], &nested) == nil {
			token = firstStringField(nested, "access_token", "accessToken", "oauth_access_token", "oauthAccessToken")
		}
	}
	if token == "" {
		return AuthMaterial{}, fmt.Errorf("Claude唤醒失败：缺少 OAuth 访问令牌")
	}
	return AuthMaterial{AccessToken: token}, nil
}

func BuildClaudeProtocol(material AuthMaterial, model, prompt string) (ProtocolRequest, error) {
	token := strings.TrimSpace(material.AccessToken)
	if token == "" {
		return ProtocolRequest{}, fmt.Errorf("Claude唤醒失败：缺少 OAuth 访问令牌")
	}
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "nexus/")
	model = strings.TrimPrefix(model, "claude/")
	if model == "" {
		return ProtocolRequest{}, fmt.Errorf("Claude唤醒失败：缺少模型名称")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = "quota activation ping"
	}
	body, err := json.Marshal(struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}{
		Model:     model,
		MaxTokens: 1,
		System:    "You are Claude Code, Anthropic's official CLI for Claude.",
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return ProtocolRequest{}, fmt.Errorf("Claude唤醒失败：编码请求失败")
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20")
	headers.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	headers.Set("X-App", "cli")
	headers.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
	return ProtocolRequest{Method: http.MethodPost, URL: ClaudeActivationURL, Headers: headers, Body: body}, nil
}

func EvaluateClaudeActivationSuccess(statusCode int, body []byte) (bool, string) {
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return false, fmt.Sprintf("Claude唤醒失败：上游返回非成功状态（HTTP %d）", statusCode)
	}
	var response struct {
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		Role       string          `json:"role"`
		StopReason string          `json:"stop_reason"`
		Error      json.RawMessage `json:"error"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil {
		return false, "Claude唤醒失败：响应不是合法数据"
	}
	if response.Type == "error" || (len(response.Error) > 0 && string(response.Error) != "null") {
		return false, "Claude唤醒失败：上游返回业务错误"
	}
	if strings.TrimSpace(response.ID) == "" || response.Type != "message" || response.Role != "assistant" ||
		response.Usage.InputTokens == nil || *response.Usage.InputTokens < 0 ||
		response.Usage.OutputTokens == nil || *response.Usage.OutputTokens < 1 {
		return false, "Claude唤醒失败：响应缺少有效输出结构"
	}
	switch response.StopReason {
	case "end_turn", "max_tokens", "stop_sequence", "refusal":
	default:
		return false, "Claude唤醒失败：响应未完成"
	}
	for _, block := range response.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			return true, ""
		}
	}
	return false, "Claude唤醒失败：响应缺少有效输出结构"
}
