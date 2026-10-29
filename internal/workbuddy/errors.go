package workbuddy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const maxErrorBodyBytes = 64 << 10

func upstreamResponseError(status int, body []byte) statusError {
	if status >= 200 && status < 300 {
		status = http.StatusBadGateway
	}
	message := fmt.Sprintf("upstream returned HTTP %d", status)
	var detail struct {
		Code        int             `json:"code"`
		Msg         string          `json:"msg"`
		Message     string          `json:"message"`
		DisplayMsg  json.RawMessage `json:"displayMsg"`
		DisplayTips json.RawMessage `json:"displayTips"`
		Error       struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(body) <= maxErrorBodyBytes && json.Unmarshal(body, &detail) == nil {
		text := firstErrorText(detail.Msg, detail.Message, detail.Error.Message, localizedErrorText(detail.DisplayMsg))
		if detail.Code != 0 {
			message = fmt.Sprintf("WorkBuddy upstream %d", detail.Code)
			if text != "" {
				message += ": " + text
			}
		} else if text != "" {
			message = text
		}
		if tip := localizedErrorText(detail.DisplayTips); tip != "" && tip != text {
			message += ": " + tip
		}
		if detail.Code == 11128 {
			status = http.StatusBadRequest
		}
		lower := strings.ToLower(text)
		if detail.Code == 6004 || strings.Contains(lower, "frequency limit") ||
			strings.Contains(text, "超出频率限制") || strings.Contains(text, "限额") {
			status = http.StatusTooManyRequests
		}
	}
	return statusErr("upstream_error", message, status == http.StatusTooManyRequests || status >= 500, status)
}

func isGatewayErrorBody(body []byte) bool {
	var detail struct {
		Code  int             `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	return len(body) <= maxErrorBodyBytes && json.Unmarshal(body, &detail) == nil &&
		(detail.Code != 0 || (len(detail.Error) > 0 && string(detail.Error) != "null"))
}

func firstErrorText(values ...string) string {
	for _, value := range values {
		if text := strings.TrimSpace(value); text != "" {
			return text
		}
	}
	return ""
}

func localizedErrorText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var localized map[string]string
	if json.Unmarshal(raw, &localized) == nil {
		return firstErrorText(localized["en"], localized["zh"], localized["zh-hant"])
	}
	return ""
}

func readStreamErrorBody(streamID string) []byte {
	if streamID == "" {
		return nil
	}
	defer closeHostHTTPStream(streamID)
	var body []byte
	// Error responses must not drain an unbounded or non-terminating stream.
	for reads := 0; reads < 64 && len(body) < maxErrorBodyBytes; reads++ {
		chunk, err := readHostHTTPStream(streamID)
		if err != nil {
			break
		}
		remaining := maxErrorBodyBytes - len(body)
		if len(chunk.Payload) > remaining {
			return nil
		}
		body = append(body, chunk.Payload...)
		if chunk.Done || chunk.Error != "" {
			break
		}
	}
	return body
}
