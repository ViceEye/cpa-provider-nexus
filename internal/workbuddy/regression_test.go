package workbuddy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ViceEye/cpa-provider-nexus/internal/pluginrpc"
)

func testJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestCredentialRegionAndRefreshStorage(t *testing.T) {
	raw := []byte(`{"type":"nexus","kind":"workbuddy-intl","refresh_token":"test-refresh","disabled":true,"priority":7,"note":"keep"}`)
	cred, err := decodeCredential(raw)
	if err != nil || cred.Region != RegionIntl || !credentialNeedsRefresh(cred) {
		t.Fatalf("invalid decoded credential: %v", err)
	}
	id := credentialID(cred)
	cred.AccessToken = "test-new-access"
	cred.RefreshToken = "test-new-refresh"
	auth, err := authDataFromCredential(cred)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(auth.StorageJSON, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["disabled"] != true || stored["priority"] != float64(7) || stored["note"] != "keep" || stored["auth_id"] != id {
		t.Fatal("refresh lost identity or host metadata")
	}
	if credentialID(cred) != id {
		t.Fatal("token rotation changed stable identity")
	}
	for _, invalid := range []string{`{"type":"other","kind":"workbuddy","api_key":"test"}`, `{"type":"nexus","kind":"workbuddy","region":"unknown","api_key":"test"}`} {
		if _, err := decodeCredential([]byte(invalid)); err == nil {
			t.Fatal("accepted invalid credential")
		}
	}
}

func TestExecuteRefreshSavesOriginalFile(t *testing.T) {
	var calls, saves int
	pluginrpc.SetCaller(func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.http.do":
			req := payload.(hostHTTPRequest)
			if req.HostCallbackID != "test-callback" {
				t.Fatal("missing callback scope")
			}
			if req.URL == CNRefreshURL {
				return testJSON(hostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`)}), nil
			}
			calls++
			if calls == 1 {
				return testJSON(hostHTTPResponse{StatusCode: 401}), nil
			}
			if req.Headers.Get("Authorization") != "Bearer new-access" {
				t.Fatal("retry did not use refreshed token")
			}
			return testJSON(hostHTTPResponse{StatusCode: 200, Body: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")}), nil
		case "host.auth.save":
			saves++
			p := payload.(map[string]any)
			if p["name"] != "original.json" {
				t.Fatal("created a different credential file")
			}
			var stored map[string]any
			if json.Unmarshal(p["json"].(json.RawMessage), &stored) != nil || stored["disabled"] != true {
				t.Fatal("lost disabled flag")
			}
			return []byte(`{}`), nil
		default:
			return nil, fmt.Errorf("unexpected callback %s", method)
		}
	})
	t.Cleanup(func() {
		pluginrpc.SetCaller(func(string, any) (json.RawMessage, error) { return nil, fmt.Errorf("test callback unset") })
	})
	req := executorRequest{AuthID: "original.json", Model: "nexus/wb/glm-5.3", HostCallbackID: "test-callback", StorageJSON: []byte(`{"type":"nexus","kind":"workbuddy","access_token":"old-access","refresh_token":"test-refresh","disabled":true}`), Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
	out, err := Execute(testJSON(req))
	var env envelope
	if err != nil || json.Unmarshal(out, &env) != nil || !env.OK || calls != 2 || saves != 1 {
		t.Fatalf("execute failed: %s %v calls=%d saves=%d", out, err, calls, saves)
	}
}

func TestAggregateRejectsInvalidOrTruncatedResponses(t *testing.T) {
	for _, raw := range []string{"", `{"code":123,"msg":"error"}`, "data: [DONE]\n", "data: bad-json\n", "data: {\"error\":{\"message\":\"denied\"}}\n", "data: {\"choices\":[{\"delta\":{\"content\":\"unfinished\"}}]}\n"} {
		if _, err := aggregateSSEChunks([]byte(raw), "test"); err == nil {
			t.Fatalf("accepted invalid response %q", raw)
		}
	}
}

func TestAggregateKeepsSparseToolCalls(t *testing.T) {
	raw := []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":3,\"id\":\"call3\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n")
	out, err := aggregateSSEChunks(raw, "test")
	if err != nil || !strings.Contains(string(out), "call3") {
		t.Fatalf("lost sparse tool call: %s %v", out, err)
	}
}

func TestTransformRejectsNonChatInput(t *testing.T) {
	for _, raw := range []string{"null", `{"input":"hello"}`, `{"messages":[],"n":2}`} {
		if _, err := transformPayload([]byte(raw), "test", RegionCN); err == nil {
			t.Fatal("accepted invalid input")
		}
	}
}

func TestStreamReadErrorIsNotSuccess(t *testing.T) {
	closed := make(chan string, 1)
	upstreamClosed := make(chan struct{}, 1)
	pluginrpc.SetCaller(func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.http.do_stream":
			return testJSON(hostHTTPStreamResponse{StatusCode: http.StatusOK, StreamID: "upstream"}), nil
		case "host.http.stream_read":
			return testJSON(hostHTTPStreamReadResponse{Error: "connection reset", Done: true}), nil
		case "host.stream.close":
			closed <- payload.(map[string]any)["error"].(string)
			return []byte(`{}`), nil
		case "host.http.stream_close":
			upstreamClosed <- struct{}{}
			return []byte(`{}`), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", method)
		}
	})
	t.Cleanup(func() {
		pluginrpc.SetCaller(func(string, any) (json.RawMessage, error) { return nil, fmt.Errorf("test callback unset") })
	})
	req := executorRequest{StreamID: "downstream", Model: "glm-5.3", StorageJSON: []byte(`{"type":"nexus","kind":"workbuddy","api_key":"test"}`), Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
	if _, err := ExecuteStream(testJSON(req)); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-closed:
		if msg != "connection reset" {
			t.Fatalf("wrong close error %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream not closed")
	}
	select {
	case <-upstreamClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not closed")
	}
}

func TestCompletionStreamAcrossEveryChunkBoundary(t *testing.T) {
	raw := []byte(": ping\r\ndata: {\"id\":\"test\",\r\ndata: \"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\r\n\r\ndata: [DONE]\r\n\r\n")
	for split := 0; split <= len(raw); split++ {
		p := &completionStream{}
		a, err := p.Feed(raw[:split])
		if err != nil {
			t.Fatal(err)
		}
		b, err := p.Feed(raw[split:])
		if err != nil {
			t.Fatal(err)
		}
		c, err := p.Finish()
		if err != nil {
			t.Fatal(err)
		}
		frames := append(append(a, b...), c...)
		if len(frames) != 1 || !json.Valid(frames[0]) || !strings.Contains(string(frames[0]), "hello") {
			t.Fatalf("split %d lost or framed data incorrectly", split)
		}
	}
}

func TestUsageMatchesConsoleSchema(t *testing.T) {
	pluginrpc.SetCaller(func(method string, payload any) (json.RawMessage, error) {
		if method != "host.http.do" {
			return nil, fmt.Errorf("unexpected method")
		}
		req := payload.(hostHTTPRequest)
		if req.URL != CNUsageURL {
			t.Fatal("wrong quota URL")
		}
		return testJSON(hostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"test-plan","Capacity":100,"CapacityUsed":25}]}}}}`)}), nil
	})
	t.Cleanup(func() {
		pluginrpc.SetCaller(func(string, any) (json.RawMessage, error) { return nil, fmt.Errorf("test callback unset") })
	})
	request := managementRequest{Body: testJSON(map[string]any{"StorageJSON": []byte(`{"type":"nexus","kind":"workbuddy","api_key":"test"}`)})}
	out, err := Usage(testJSON(request))
	if err != nil {
		t.Fatal(err)
	}
	var env struct{ Result managementResponse }
	if json.Unmarshal(out, &env) != nil {
		t.Fatal("invalid envelope")
	}
	var body struct {
		Accounts []struct {
			Usage []struct {
				CurrentUsage float64 `json:"current_usage"`
				UsageLimit   float64 `json:"usage_limit"`
				Remaining    float64 `json:"remaining"`
			}
		}
	}
	if json.Unmarshal(env.Result.Body, &body) != nil || len(body.Accounts) != 1 || len(body.Accounts[0].Usage) != 1 {
		t.Fatal("missing console usage")
	}
	u := body.Accounts[0].Usage[0]
	if u.CurrentUsage != 25 || u.UsageLimit != 100 || u.Remaining != 75 {
		t.Fatal("incorrect console quota")
	}
}

func TestLoginPendingThenSuccessUsesRegion(t *testing.T) {
	polls := 0
	pluginrpc.SetCaller(func(method string, payload any) (json.RawMessage, error) {
		if method != "host.http.do" {
			return nil, fmt.Errorf("unexpected method")
		}
		req := payload.(hostHTTPRequest)
		if strings.HasPrefix(req.URL, IntlStateURL) {
			if req.Method != http.MethodPost {
				t.Fatal("wrong state method")
			}
			return testJSON(hostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":0,"data":{"state":"test-state","authUrl":"https://www.codebuddy.ai/"}}`)}), nil
		}
		if req.URL != IntlTokenURL+"?state=test-state" || req.Method != http.MethodGet {
			t.Fatal("poll used wrong region or method")
		}
		polls++
		if polls == 1 {
			return testJSON(hostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":11217}`)}), nil
		}
		return testJSON(hostHTTPResponse{StatusCode: 200, Body: []byte(`{"code":0,"data":{"accessToken":"test-access","refreshToken":"test-refresh","expiresIn":3600}}`)}), nil
	})
	t.Cleanup(func() {
		pluginrpc.SetCaller(func(string, any) (json.RawMessage, error) { return nil, fmt.Errorf("test callback unset") })
	})
	start, err := LoginStart([]byte(`{"Metadata":{"login_mode":"workbuddy-intl"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if json.Unmarshal(start, &env) != nil || !env.OK {
		t.Fatal("login start failed")
	}
	for _, want := range []string{"pending", "success"} {
		out, err := LoginPoll([]byte(`{"State":"test-state"}`))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Result struct {
				Status string
				Auth   authData
			}
		}
		if json.Unmarshal(out, &result) != nil || result.Result.Status != want {
			t.Fatalf("expected %s", want)
		}
		if want == "success" {
			c, err := decodeCredential(result.Result.Auth.StorageJSON)
			if err != nil || c.Region != RegionIntl || c.Type != pluginProvider {
				t.Fatal("invalid login credential")
			}
		}
	}
}
