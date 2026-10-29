package workbuddy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ViceEye/cpa-provider-nexus/internal/pluginrpc"
)

var channelUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func checkChannelHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	for _, key := range []string{"X-Request-ID", "X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"} {
		if !channelUUIDPattern.MatchString(headers.Get(key)) {
			t.Fatalf("%s must be a UUID v4", key)
		}
	}
	requestID := headers.Get("X-Request-ID")
	if requestID != headers.Get("X-Conversation-Request-ID") || requestID != headers.Get("X-Conversation-Message-ID") {
		t.Fatal("request and conversation request/message ids must match")
	}
	if requestID == headers.Get("X-Conversation-ID") {
		t.Fatal("conversation id must be independent of request id")
	}
}

func TestChatChannelHeaders(t *testing.T) {
	for _, region := range []string{RegionCN, RegionIntl} {
		t.Run(region, func(t *testing.T) {
			cred := credential{Region: region, AccessToken: "test-access"}
			first := buildWorkBuddyHeaders(cred, true)
			second := buildWorkBuddyHeaders(cred, true)
			checkChannelHeaders(t, first)
			checkChannelHeaders(t, second)
			for _, key := range []string{"X-Request-ID", "X-Conversation-ID"} {
				if first.Get(key) == second.Get(key) {
					t.Fatalf("%s reused across requests", key)
				}
			}
			if first.Get("Authorization") != "Bearer test-access" || first.Get("Accept") != "text/event-stream" {
				t.Fatal("channel headers lost auth or stream headers")
			}
			if buildWorkBuddyHeaders(cred, false).Get("X-Request-ID") != "" {
				t.Fatal("quota request unexpectedly received chat identifiers")
			}
		})
	}
}

func setWorkBuddyTestCaller(t *testing.T, caller pluginrpc.Caller) {
	t.Helper()
	pluginrpc.SetCaller(caller)
	t.Cleanup(func() {
		pluginrpc.SetCaller(func(string, any) (json.RawMessage, error) {
			return nil, fmt.Errorf("test callback unset")
		})
	})
}

func TestExecutorsSendChannelHeadersAndNormalizeAliases(t *testing.T) {
	for _, region := range []string{RegionCN, RegionIntl} {
		for _, model := range []string{"nexus/deepseek-v4.1-flash", "nexus/wb/deepseek-v4.1-flash"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", region, model, stream), func(t *testing.T) {
					calls, closed := 0, 0
					setWorkBuddyTestCaller(t, func(method string, payload any) (json.RawMessage, error) {
						switch method {
						case "host.http.do", "host.http.do_stream":
							calls++
							request := payload.(hostHTTPRequest)
							checkChannelHeaders(t, request.Headers)
							if request.URL != getChatURL(region) || request.HostCallbackID != "callback" {
								t.Fatal("request lost region or callback scope")
							}
							var body map[string]any
							if json.Unmarshal(request.Body, &body) != nil || body["model"] != "deepseek-v4.1-flash" || body["stream"] != true {
								t.Fatal("incorrect upstream model or stream mode")
							}
							if method == "host.http.do_stream" {
								return testJSON(hostHTTPStreamResponse{StatusCode: 400, StreamID: "error-stream"}), nil
							}
							return testJSON(hostHTTPResponse{StatusCode: 400, Body: []byte(`{"code":11128,"msg":"request illegal"}`)}), nil
						case "host.http.stream_read":
							return testJSON(hostHTTPStreamReadResponse{Payload: []byte(`{"code":11128,"msg":"request illegal"}`), Done: true}), nil
						case "host.http.stream_close":
							closed++
							return []byte(`{}`), nil
						default:
							return nil, fmt.Errorf("unexpected method %s", method)
						}
					})
					req := executorRequest{
						Model: model, StreamID: "downstream", HostCallbackID: "callback",
						StorageJSON: testJSON(credential{Type: pluginProvider, Kind: TypeMarker, Region: region, APIKey: "test-key"}),
						Payload:     []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
					}
					execute := Execute
					if stream {
						execute = ExecuteStream
					}
					raw, err := execute(testJSON(req))
					var env envelope
					if err != nil || json.Unmarshal(raw, &env) != nil || env.OK || env.Error == nil {
						t.Fatalf("expected upstream error: %s %v", raw, err)
					}
					if env.Error.HTTPStatus != 400 || env.Error.Retryable || !strings.Contains(env.Error.Message, "11128") {
						t.Fatalf("lost channel error details: %+v", env.Error)
					}
					if calls != 1 || (stream && closed != 1) {
						t.Fatalf("channel rejection retried or stream leaked: calls=%d closed=%d", calls, closed)
					}
				})
			}
		}
	}
}

func TestStreamRefreshRebuildsChannelHeaders(t *testing.T) {
	var previous http.Header
	calls, refreshes, saves, closed := 0, 0, 0, 0
	setWorkBuddyTestCaller(t, func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.http.do_stream":
			calls++
			req := payload.(hostHTTPRequest)
			checkChannelHeaders(t, req.Headers)
			if calls == 1 {
				previous = req.Headers
				return testJSON(hostHTTPStreamResponse{StatusCode: 401, StreamID: "expired"}), nil
			}
			if req.Headers.Get("X-Request-ID") == previous.Get("X-Request-ID") || req.Headers.Get("Authorization") != "Bearer refreshed" {
				t.Fatal("retry reused identifiers or stale token")
			}
			return testJSON(hostHTTPStreamResponse{StatusCode: 400, StreamID: "rejected"}), nil
		case "host.http.do":
			if payload.(hostHTTPRequest).URL != IntlRefreshURL {
				t.Fatal("unexpected refresh URL")
			}
			refreshes++
			return testJSON(hostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":0,"data":{"accessToken":"refreshed","refreshToken":"refresh","expiresIn":3600}}`)}), nil
		case "host.auth.save":
			saves++
			if payload.(map[string]any)["name"] != "original.json" {
				t.Fatal("refresh changed credential file")
			}
			return []byte(`{}`), nil
		case "host.http.stream_read":
			return testJSON(hostHTTPStreamReadResponse{Payload: []byte(`{"code":11128,"msg":"request illegal"}`), Done: true}), nil
		case "host.http.stream_close":
			closed++
			return []byte(`{}`), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", method)
		}
	})
	req := executorRequest{
		AuthID: "original.json", StreamID: "downstream", Model: "nexus/wb/deepseek-v4.1-flash",
		StorageJSON: []byte(`{"type":"nexus","kind":"workbuddy","region":"intl","access_token":"old","refresh_token":"refresh"}`),
		Payload:     []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
	}
	raw, err := ExecuteStream(testJSON(req))
	var env envelope
	if err != nil || json.Unmarshal(raw, &env) != nil || env.Error == nil || env.Error.HTTPStatus != 400 ||
		calls != 2 || refreshes != 1 || saves != 1 || closed != 2 {
		t.Fatalf("refresh/stream lifecycle failed: calls=%d refreshes=%d saves=%d closed=%d, %s %v", calls, refreshes, saves, closed, raw, err)
	}
}
