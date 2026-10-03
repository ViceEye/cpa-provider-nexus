package provider

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestPublicResourcesNeverProcessOAuth(t *testing.T) {
	originalHTTP := hostHTTPDoCall
	originalCall := callHostCall
	t.Cleanup(func() { hostHTTPDoCall = originalHTTP; callHostCall = originalCall })
	callHostCall = func(method string, payload any) (json.RawMessage, error) {
		t.Fatalf("public resource invoked host callback %s", method)
		return nil, nil
	}
	hostHTTPDoCall = func(req hostHTTPRequest) (hostHTTPResponse, error) {
		t.Fatal("public resource invoked upstream HTTP")
		return hostHTTPResponse{}, nil
	}
	state := managementBrowserSession(t, time.Now().UTC().Add(time.Minute))
	before, _ := browserLoginSessionForState(state)
	for _, path := range []string{
		"/v0/resource/plugins/cpa-provider-nexus/oauth",
		"/v0/resource/plugins/cpa-provider-nexus/oauth/",
		"/v0/resource/plugins/cpa-provider-nexus/oauth/signin/callback",
		"/v0/resource/plugins/cpa-provider-nexus/oauth/oauth/callback",
		"/v0/resource/plugins/cpa-provider-nexus/console/oauth/start",
		"/v0/resource/plugins/cpa-provider-nexus/quota",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, query := range []map[string][]string{
				{"state": {state}, "code": {"fixture-code"}},
				{"state": {state}, "error": {"access_denied"}},
				{"state": {state}, "login_option": {"builderid"}},
			} {
				response := handleManagementResponse(t, managementRequest{Method: method, Path: path, Query: query})
				if response.StatusCode != http.StatusNotFound || response.Headers.Get("Location") != "" {
					t.Fatalf("public resource %s %s returned %d", method, path, response.StatusCode)
				}
				after, exists := browserLoginSessionForState(state)
				if !exists || !reflect.DeepEqual(before, after) {
					t.Fatal("public resource mutated login state")
				}
			}
		}
	}
	for _, path := range []string{nexusLogoPath, "/v0/resource/plugins/cpa-provider-nexus/console"} {
		response := handleManagementResponse(t, managementRequest{Method: http.MethodGet, Path: path})
		if response.StatusCode != http.StatusOK || len(response.Body) == 0 {
			t.Fatalf("static resource unavailable: %s", path)
		}
	}
}
