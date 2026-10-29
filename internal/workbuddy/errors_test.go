package workbuddy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestUpstreamResponseError(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantHTTP  int
		retryable bool
		contains  []string
	}{
		{"channel", 400, `{"code":11128,"msg":"request illegal","displayTips":{"en":"This call failed channel verification."}}`, 400, false, []string{"11128", "request illegal", "channel verification"}},
		{"malformed request", 400, `{"code":11128,"msg":"first message is not system prompt"}`, 400, false, []string{"11128", "first message"}},
		{"frequency code", 400, `{"code":6004,"message":"Try again later"}`, 429, true, []string{"6004", "Try again later"}},
		{"frequency code in HTTP 200", 200, `{"code":6004,"msg":"frequency limit exceeded"}`, 429, true, []string{"6004"}},
		{"channel in HTTP 200", 200, `{"code":11128,"msg":"request illegal"}`, 400, false, []string{"11128"}},
		{"frequency text", 400, `{"code":11000,"msg":"frequency limit exceeded"}`, 429, true, []string{"11000", "frequency limit"}},
		{"nested", 502, `{"error":{"message":"upstream unavailable"}}`, 502, true, []string{"upstream unavailable"}},
		{"localized", 403, `{"displayMsg":{"zh":"blocked"},"displayTips":"use official client"}`, 403, false, []string{"blocked", "use official client"}},
		{"empty", 429, "", 429, true, []string{"HTTP 429"}},
		{"invalid", 503, `<html>secret should not be copied</html>`, 503, true, []string{"HTTP 503"}},
		{"oversized", 400, `{"msg":"` + strings.Repeat("x", maxErrorBodyBytes) + `"}`, 400, false, []string{"HTTP 400"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upstreamResponseError(tt.status, []byte(tt.body))
			if got.HTTPStatus != tt.wantHTTP || got.Retryable != tt.retryable {
				t.Fatalf("unexpected error status: %+v", got)
			}
			for _, part := range tt.contains {
				if !strings.Contains(got.Message, part) {
					t.Fatalf("message %q does not contain %q", got.Message, part)
				}
			}
			if strings.Contains(got.Message, "secret should not be copied") {
				t.Fatal("copied unstructured error body")
			}
		})
	}
}

func TestStreamErrorsPreserveBodyAndMapFrequencyLimit(t *testing.T) {
	for _, body := range []string{
		`{"code":6004,"msg":"frequency limit exceeded"}`,
		`{"code":11128,"msg":"request illegal","displayTips":{"en":"channel verification failed"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			reads, closed := 0, 0
			setWorkBuddyTestCaller(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case "host.http.do_stream":
					return testJSON(hostHTTPStreamResponse{StatusCode: 400, StreamID: "upstream"}), nil
				case "host.http.stream_read":
					reads++
					chunk := body[:len(body)/2]
					if reads == 2 {
						chunk = body[len(body)/2:]
					}
					return testJSON(hostHTTPStreamReadResponse{Payload: []byte(chunk), Done: reads == 2}), nil
				case "host.http.stream_close":
					closed++
					return []byte(`{}`), nil
				default:
					return nil, fmt.Errorf("unexpected method %s", method)
				}
			})
			raw, err := ExecuteStream(testJSON(executorRequest{
				StreamID: "client", Model: "nexus/deepseek-v4.1-flash",
				StorageJSON: []byte(`{"type":"nexus","kind":"workbuddy","region":"intl","api_key":"test"}`),
				Payload:     []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			}))
			var env envelope
			if err != nil || json.Unmarshal(raw, &env) != nil || env.Error == nil || reads != 2 || closed != 1 {
				t.Fatalf("stream error lifecycle failed: %s %v reads=%d closed=%d", raw, err, reads, closed)
			}
			want := upstreamResponseError(400, []byte(body))
			if env.Error.Message != want.Message || env.Error.HTTPStatus != want.HTTPStatus || env.Error.Retryable != want.Retryable {
				t.Fatalf("stream lost upstream error: %+v, want %+v", env.Error, want)
			}
		})
	}
}

func TestHTTP200GatewayErrorIsNotACompletion(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			const body = `{"code":6004,"msg":"frequency limit exceeded"}`
			closed := 0
			setWorkBuddyTestCaller(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case "host.http.do":
					return testJSON(hostHTTPResponse{StatusCode: 200, Body: []byte(body)}), nil
				case "host.http.do_stream":
					return testJSON(hostHTTPStreamResponse{
						StatusCode: 200, StreamID: "error",
						Headers: map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
					}), nil
				case "host.http.stream_read":
					return testJSON(hostHTTPStreamReadResponse{Payload: []byte(body), Done: true}), nil
				case "host.http.stream_close":
					closed++
					return []byte(`{}`), nil
				default:
					return nil, fmt.Errorf("unexpected method %s", method)
				}
			})
			req := executorRequest{
				StreamID: "client", Model: "nexus/wb/deepseek-v4.1-flash",
				StorageJSON: []byte(`{"type":"nexus","kind":"workbuddy","region":"intl","api_key":"test"}`),
				Payload:     []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			}
			execute := Execute
			if stream {
				execute = ExecuteStream
			}
			raw, err := execute(testJSON(req))
			var env envelope
			if err != nil || json.Unmarshal(raw, &env) != nil || env.OK || env.Error == nil ||
				env.Error.HTTPStatus != 429 || !env.Error.Retryable || !strings.Contains(env.Error.Message, "6004") {
				t.Fatalf("HTTP 200 error treated as completion: %s %v", raw, err)
			}
			if stream && closed != 1 {
				t.Fatal("JSON error stream not closed")
			}
		})
	}
}

func TestReadStreamErrorBodyBoundsAndCloses(t *testing.T) {
	for _, mode := range []string{"oversized", "empty", "read failure", "chunk failure"} {
		t.Run(mode, func(t *testing.T) {
			reads, closed := 0, 0
			setWorkBuddyTestCaller(t, func(method string, payload any) (json.RawMessage, error) {
				switch method {
				case "host.http.stream_read":
					reads++
					switch mode {
					case "oversized":
						return testJSON(hostHTTPStreamReadResponse{Payload: []byte(strings.Repeat("x", maxErrorBodyBytes+1))}), nil
					case "read failure":
						return nil, fmt.Errorf("read failed")
					case "chunk failure":
						return testJSON(hostHTTPStreamReadResponse{Error: "reset"}), nil
					default:
						return testJSON(hostHTTPStreamReadResponse{}), nil
					}
				case "host.http.stream_close":
					closed++
					return []byte(`{}`), nil
				default:
					return nil, fmt.Errorf("unexpected method %s", method)
				}
			})
			body := readStreamErrorBody("error-stream")
			if len(body) != 0 || reads > 64 || closed != 1 {
				t.Fatalf("unbounded/leaked stream: bytes=%d reads=%d closed=%d", len(body), reads, closed)
			}
		})
	}
}
