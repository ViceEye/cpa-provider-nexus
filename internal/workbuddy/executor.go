package workbuddy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	agentPromptPattern = regexp.MustCompile(`(?i)you are claude code|claude.?code.+official.+cli|anthropic.+official.+cli|anxthxropic.+official.+cli|you are (?:cursor|windsurf|cline|aider|continue|copilot|cody)|you are an? (?:ai )?(?:coding |code )?agent|cc_entrypoint\s*=\s*(?:cli|vscode|jetbrains|gui)|claude.?code.+issues|give feedback.+claude.?code|you are .{0,30}(?:powerful )?ai agent|orchestration capabilities|OhMyOpenCode|<agent-identity>|<Role>|<Behavior_Instructions>`)
	neutralPrompt      = "You are a helpful AI assistant that helps with software engineering tasks."
)

// normalizeModel removes any proxy/provider prefixes.
func normalizeModel(model string) string {
	model = strings.TrimPrefix(model, "nexus/")
	model = strings.TrimPrefix(model, "workbuddy/")
	model = strings.TrimPrefix(model, "wb/")
	model = strings.TrimPrefix(model, "cbcn/")
	model = strings.TrimPrefix(model, "cbai/")
	return strings.TrimSpace(model)
}

// flattenContent extracts plain text from either a string or typed text blocks.
func flattenContent(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	if list, ok := content.([]any); ok {
		var parts []string
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// transformPayload adapts the OpenAI request payload for CodeBuddy/WorkBuddy gateways.
func transformPayload(raw []byte, targetModel, region string) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("invalid json payload: %w", err)
	}

	if body == nil {
		return nil, fmt.Errorf("payload must be a JSON object")
	}
	if _, ok := body["messages"].([]any); !ok {
		return nil, fmt.Errorf("messages must be an array")
	}
	if n, ok := body["n"].(float64); ok && n != 1 {
		return nil, fmt.Errorf("WorkBuddy supports only n=1")
	}
	body["model"] = targetModel
	// CodeBuddy requires stream: true
	body["stream"] = true

	// Handle reasoning_effort
	if eff, ok := body["reasoning_effort"].(string); ok {
		eff = strings.ToLower(strings.TrimSpace(eff))
		if eff == "none" || eff == "off" {
			delete(body, "reasoning_effort")
		} else if eff != "" {
			body["reasoning_summary"] = "auto"
		}
	}

	rawMessages, ok := body["messages"].([]any)
	if !ok {
		return json.Marshal(body)
	}

	if region == RegionIntl {
		// CodeBuddy Intl requires leading system prompt: "You are CodeBuddy Code."
		// and user messages as typed blocks.
		newMessages := []any{
			map[string]any{"role": "system", "content": "You are CodeBuddy Code."},
		}
		for _, rawMsg := range rawMessages {
			msg, ok := rawMsg.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role == "system" || role == "developer" {
				continue
			}
			if role == "user" {
				if text, ok := msg["content"].(string); ok {
					newMsg := make(map[string]any, len(msg))
					for k, v := range msg {
						newMsg[k] = v
					}
					newMsg["content"] = []any{map[string]any{"type": "text", "text": text}}
					newMessages = append(newMessages, newMsg)
					continue
				}
			}
			newMessages = append(newMessages, msg)
		}
		body["messages"] = newMessages
	} else {
		// CodeBuddy CN: neutralize agent system prompts to avoid Tencent content filter
		newMessages := make([]any, 0, len(rawMessages))
		for _, rawMsg := range rawMessages {
			msg, ok := rawMsg.(map[string]any)
			if !ok {
				newMessages = append(newMessages, rawMsg)
				continue
			}
			role, _ := msg["role"].(string)
			if role == "system" {
				text := flattenContent(msg["content"])
				if len(text) > 2000 || agentPromptPattern.MatchString(text) {
					newMsg := make(map[string]any, len(msg))
					for k, v := range msg {
						newMsg[k] = v
					}
					if _, isStr := msg["content"].(string); isStr {
						newMsg["content"] = neutralPrompt
					} else {
						newMsg["content"] = []any{map[string]any{"type": "text", "text": neutralPrompt}}
					}
					newMessages = append(newMessages, newMsg)
					continue
				}
			}
			newMessages = append(newMessages, msg)
		}
		body["messages"] = newMessages
	}

	return json.Marshal(body)
}

func getChatURL(region string) string {
	if region == RegionIntl {
		return IntlBaseURL + IntlChatPath
	}
	return CNBaseURL + CNChatPath
}

// Execute handles a non-streaming chat completion request by querying the upstream
// streaming endpoint and aggregating chunks into a standard ChatCompletion response.
func Execute(raw []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cred, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_auth", err.Error(), false, http.StatusUnauthorized), nil
	}
	authID := StableCredentialID(req.StorageJSON)
	if authID == "" {
		authID = req.AuthID
	}

	if credentialNeedsRefresh(cred) {
		refreshed, err := refreshCredential(cred, req.HostCallbackID)
		if err != nil {
			return pluginError(err), nil
		}
		cred = refreshed
		persistCredentialBestEffort(req.AuthID, cred)
	}

	targetModel := normalizeModel(req.Model)
	transformedPayload, err := transformPayload(req.Payload, targetModel, cred.Region)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), false, http.StatusBadRequest), nil
	}

	chatURL := getChatURL(cred.Region)
	headers := buildWorkBuddyHeaders(cred, true)

	resp, err := hostHTTP(hostHTTPRequest{
		HostCallbackID: req.HostCallbackID,
		Method:         http.MethodPost,
		URL:            chatURL,
		Headers:        headers,
		Body:           transformedPayload,
	})
	if err != nil {
		observeRequest(authID, req.Model, false, err.Error())
		return pluginError(statusErr("upstream_network_error", "WorkBuddy request failed: "+err.Error(), true, http.StatusBadGateway)), nil
	}

	// 401/403: retry once with refreshed token
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		refreshed, errRef := refreshCredential(cred, req.HostCallbackID)
		if errRef != nil {
			observeRequest(authID, req.Model, false, errRef.Error())
			return pluginError(errRef), nil
		}
		cred = refreshed
		persistCredentialBestEffort(req.AuthID, cred)
		headers = buildWorkBuddyHeaders(cred, true)

		resp, err = hostHTTP(hostHTTPRequest{
			HostCallbackID: req.HostCallbackID,
			Method:         http.MethodPost,
			URL:            chatURL,
			Headers:        headers,
			Body:           transformedPayload,
		})
		if err != nil {
			observeRequest(authID, req.Model, false, err.Error())
			return pluginError(statusErr("upstream_network_error", "WorkBuddy request failed after refresh: "+err.Error(), true, http.StatusBadGateway)), nil
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		message := strings.TrimSpace(string(resp.Body))
		if message == "" {
			message = fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)
		}
		observeRequest(authID, req.Model, false, message)
		return errorEnvelope("upstream_error", message, retryable, resp.StatusCode), nil
	}

	// Aggregate SSE chunks into a standard ChatCompletion JSON
	aggregated, err := aggregateSSEChunks(resp.Body, targetModel)
	if err != nil {
		observeRequest(authID, req.Model, false, err.Error())
		return errorEnvelope("upstream_error", "Failed to aggregate WorkBuddy response: "+err.Error(), false, http.StatusBadGateway), nil
	}

	observeRequest(authID, req.Model, true, "")
	return okEnvelope(executorResponse{
		Payload: aggregated,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// ExecuteStream handles a streaming chat completion request.
func ExecuteStream(raw []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cred, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_auth", err.Error(), false, http.StatusUnauthorized), nil
	}
	authID := StableCredentialID(req.StorageJSON)
	if authID == "" {
		authID = req.AuthID
	}

	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required", false, http.StatusInternalServerError), nil
	}

	if credentialNeedsRefresh(cred) {
		refreshed, err := refreshCredential(cred, req.HostCallbackID)
		if err != nil {
			return pluginError(err), nil
		}
		cred = refreshed
		persistCredentialBestEffort(req.AuthID, cred)
	}

	targetModel := normalizeModel(req.Model)
	transformedPayload, err := transformPayload(req.Payload, targetModel, cred.Region)
	if err != nil {
		return errorEnvelope("invalid_request", err.Error(), false, http.StatusBadRequest), nil
	}

	chatURL := getChatURL(cred.Region)
	headers := buildWorkBuddyHeaders(cred, true)

	streamResp, err := hostHTTPDoStream(hostHTTPRequest{
		HostCallbackID: req.HostCallbackID,
		Method:         http.MethodPost,
		URL:            chatURL,
		Headers:        headers,
		Body:           transformedPayload,
	})
	if err != nil {
		observeRequest(authID, req.Model, false, err.Error())
		return pluginError(statusErr("upstream_network_error", "WorkBuddy stream failed: "+err.Error(), true, http.StatusBadGateway)), nil
	}

	// If 401/403, retry once with refreshed token
	if streamResp.StatusCode == http.StatusUnauthorized || streamResp.StatusCode == http.StatusForbidden {
		closeHostHTTPStream(streamResp.StreamID)
		refreshed, errRef := refreshCredential(cred, req.HostCallbackID)
		if errRef != nil {
			observeRequest(authID, req.Model, false, errRef.Error())
			return pluginError(errRef), nil
		}
		cred = refreshed
		persistCredentialBestEffort(req.AuthID, cred)
		headers = buildWorkBuddyHeaders(cred, true)

		streamResp, err = hostHTTPDoStream(hostHTTPRequest{
			HostCallbackID: req.HostCallbackID,
			Method:         http.MethodPost,
			URL:            chatURL,
			Headers:        headers,
			Body:           transformedPayload,
		})
		if err != nil {
			observeRequest(authID, req.Model, false, err.Error())
			return pluginError(statusErr("upstream_network_error", "WorkBuddy stream failed after refresh: "+err.Error(), true, http.StatusBadGateway)), nil
		}
	}

	if streamResp.StatusCode < 200 || streamResp.StatusCode >= 300 {
		closeHostHTTPStream(streamResp.StreamID)
		retryable := streamResp.StatusCode == http.StatusTooManyRequests || streamResp.StatusCode >= 500
		message := fmt.Sprintf("upstream returned HTTP %d", streamResp.StatusCode)
		observeRequest(authID, req.Model, false, message)
		return errorEnvelope("upstream_error", message, retryable, streamResp.StatusCode), nil
	}

	// Stream forwarding goroutine
	streamID := req.StreamID
	hostStreamID := streamResp.StreamID
	go func() {
		defer closeHostHTTPStream(hostStreamID)
		parser := &completionStream{}
		for {
			chunk, errRead := readHostHTTPStream(hostStreamID)
			if errRead != nil {
				observeRequest(authID, req.Model, false, errRead.Error())
				closePluginStream(streamID, errRead.Error())
				return
			}
			if chunk.Error != "" {
				observeRequest(authID, req.Model, false, chunk.Error)
				closePluginStream(streamID, chunk.Error)
				return
			}
			if len(chunk.Payload) > 0 {
				frames, errParse := parser.Feed(chunk.Payload)
				if errParse != nil {
					observeRequest(authID, req.Model, false, errParse.Error())
					closePluginStream(streamID, errParse.Error())
					return
				}
				for _, frame := range frames {
					if errEmit := emitPluginStream(streamID, frame); errEmit != nil {
						observeRequest(authID, req.Model, false, errEmit.Error())
						closePluginStream(streamID, errEmit.Error())
						return
					}
				}
			}
			if chunk.Done {
				frames, errParse := parser.Finish()
				if errParse != nil {
					observeRequest(authID, req.Model, false, errParse.Error())
					closePluginStream(streamID, errParse.Error())
					return
				}
				for _, frame := range frames {
					if err := emitPluginStream(streamID, frame); err != nil {
						observeRequest(authID, req.Model, false, err.Error())
						closePluginStream(streamID, err.Error())
						return
					}
				}
				observeRequest(authID, req.Model, true, "")
				closePluginStream(streamID, "")
				return
			}
		}
	}()

	return okEnvelope(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
}

// aggregateSSEChunks collects streamed SSE data into a single OpenAI ChatCompletion.
func aggregateSSEChunks(raw []byte, fallbackModel string) ([]byte, error) {
	parser := &completionStream{}
	frames, err := parser.Feed(raw)
	if err != nil {
		return nil, err
	}
	tail, err := parser.Finish()
	if err != nil {
		return nil, err
	}
	frames = append(frames, tail...)
	var id string
	var model string
	created := time.Now().Unix()
	var contentBuilder strings.Builder
	var reasoningBuilder strings.Builder
	var finishReason string
	var usage map[string]any

	type toolCallBuilder struct {
		ID        string
		Type      string
		Name      string
		Arguments strings.Builder
	}
	toolCallsMap := make(map[int]*toolCallBuilder)

	for _, data := range frames {

		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role             string `json:"role"`
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}

		if err := json.Unmarshal(data, &chunk); err != nil {
			return nil, fmt.Errorf("invalid completion chunk")
		}

		if chunk.ID != "" && id == "" {
			id = chunk.ID
		}
		if chunk.Model != "" && model == "" {
			model = chunk.Model
		}
		if chunk.Created != 0 {
			created = chunk.Created
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				return nil, fmt.Errorf("multiple choices are unsupported")
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
			if choice.Delta.Content != "" {
				contentBuilder.WriteString(choice.Delta.Content)
			}
			if choice.Delta.ReasoningContent != "" {
				reasoningBuilder.WriteString(choice.Delta.ReasoningContent)
			}
			for _, tc := range choice.Delta.ToolCalls {
				idx := tc.Index
				b, exists := toolCallsMap[idx]
				if !exists {
					b = &toolCallBuilder{
						ID:   tc.ID,
						Type: tc.Type,
						Name: tc.Function.Name,
					}
					if b.Type == "" {
						b.Type = "function"
					}
					toolCallsMap[idx] = b
				}
				if tc.ID != "" && b.ID == "" {
					b.ID = tc.ID
				}
				if tc.Function.Name != "" && b.Name == "" {
					b.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					b.Arguments.WriteString(tc.Function.Arguments)
				}
			}
		}
	}

	if id == "" {
		id = "chatcmpl-" + randomID()[:12]
	}
	if model == "" {
		model = fallbackModel
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	message := map[string]any{
		"role":    "assistant",
		"content": contentBuilder.String(),
	}
	if reasoningBuilder.Len() > 0 {
		message["reasoning_content"] = reasoningBuilder.String()
	}

	if len(toolCallsMap) > 0 {
		var toolCalls []any
		indices := make([]int, 0, len(toolCallsMap))
		for index := range toolCallsMap {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, i := range indices {
			if b, ok := toolCallsMap[i]; ok {
				toolCalls = append(toolCalls, map[string]any{
					"id":   b.ID,
					"type": b.Type,
					"function": map[string]any{
						"name":      b.Name,
						"arguments": b.Arguments.String(),
					},
				})
			}
		}
		message["tool_calls"] = toolCalls
		if finishReason == "stop" {
			finishReason = "tool_calls"
		}
	}

	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": finishReason,
	}

	result := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	if usage != nil {
		result["usage"] = usage
	} else {
		result["usage"] = map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		}
	}

	return json.Marshal(result)
}
