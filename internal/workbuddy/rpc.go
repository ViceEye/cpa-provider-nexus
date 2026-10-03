package workbuddy

import (
	"encoding/json"
	"net/http"

	"github.com/ViceEye/cpa-provider-nexus/internal/pluginrpc"
)

type hostHTTPRequest = pluginrpc.HTTPRequest
type hostHTTPResponse = pluginrpc.HTTPResponse
type hostHTTPStreamResponse = pluginrpc.HTTPStreamResponse
type hostHTTPStreamReadResponse = pluginrpc.HTTPStreamReadResponse

var requestObserver func(authID, model string, success bool, message string)

// SetRequestObserver lets the provider package record completed WorkBuddy model
// requests without creating an import cycle.
func SetRequestObserver(observer func(authID, model string, success bool, message string)) {
	requestObserver = observer
}

func observeRequest(authID, model string, success bool, message string) {
	if requestObserver != nil {
		requestObserver(authID, model, success, message)
	}
}

func hostHTTP(req hostHTTPRequest) (hostHTTPResponse, error) {
	return pluginrpc.Do(req)
}

func callHost(method string, payload any) (json.RawMessage, error) {
	return pluginrpc.Call(method, payload)
}

func hostHTTPDoStream(req hostHTTPRequest) (hostHTTPStreamResponse, error) {
	return pluginrpc.DoStream(req)
}

func emitPluginStream(streamID string, payload []byte) error {
	return pluginrpc.EmitStream(streamID, payload)
}

func closePluginStream(streamID, errorMessage string) {
	pluginrpc.ClosePluginStream(streamID, errorMessage)
}

func readHostHTTPStream(streamID string) (hostHTTPStreamReadResponse, error) {
	return pluginrpc.ReadStream(streamID)
}

func readAllHostHTTPStream(streamID string) ([]byte, error) {
	return pluginrpc.ReadAllStream(streamID)
}

func closeHostHTTPStream(streamID string) {
	pluginrpc.CloseStream(streamID)
}

type envelope = pluginrpc.Envelope
type envelopeError = pluginrpc.EnvelopeError

func okEnvelope(value any) ([]byte, error) {
	return pluginrpc.OK(value)
}

func errorEnvelope(code, message string, retryable bool, status int) []byte {
	return pluginrpc.Error(code, message, retryable, status)
}

func pluginError(err error) []byte {
	if err == nil {
		return errorEnvelope("plugin_error", "unknown error", false, http.StatusInternalServerError)
	}
	if serr, ok := err.(statusError); ok {
		return errorEnvelope(serr.Code, serr.Message, serr.Retryable, serr.HTTPStatus)
	}
	return errorEnvelope("plugin_error", err.Error(), false, http.StatusInternalServerError)
}

func managementJSON(status int, body map[string]any) []byte {
	return pluginrpc.ManagementJSON(status, body)
}
