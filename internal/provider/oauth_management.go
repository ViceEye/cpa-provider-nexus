package provider

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ViceEye/cpa-provider-nexus/internal/jsonx"
)

type browserCallbackManagementRequest struct {
	RedirectURL string `json:"redirect_url"`
}

type browserCallbackOutcome struct {
	Status      string
	State       string
	URL         string
	UserCode    string
	ExpiresAt   time.Time
	LoginOption string
	OAuthError  bool
}

func handleBrowserCallbackManagement(req managementRequest) ([]byte, error) {
	if !strings.EqualFold(req.Method, http.MethodPost) {
		return okEnvelope(managementResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    jsonHeaders(),
			Body:       mustJSON(map[string]any{"error": "method_not_allowed"}),
		})
	}
	var body browserCallbackManagementRequest
	if errDecode := json.Unmarshal(req.Body, &body); errDecode != nil {
		return browserCallbackError(http.StatusBadRequest, "invalid_callback", "Kiro callback request must contain JSON")
	}
	callbackURL, errParse := url.Parse(strings.TrimSpace(body.RedirectURL))
	if errParse != nil || callbackURL.Scheme == "" || callbackURL.Host == "" {
		return browserCallbackError(http.StatusBadRequest, "invalid_callback", "Kiro callback URL is invalid")
	}
	outcome, errCallback := processBrowserCallback(callbackURL, req.HostCallbackID)
	if errCallback != nil {
		return browserCallbackErrorResponse(errCallback)
	}
	bodyResponse := map[string]any{"status": outcome.Status, "state": outcome.State}
	if outcome.URL != "" {
		bodyResponse["url"] = outcome.URL
		bodyResponse["user_code"] = outcome.UserCode
		bodyResponse["expires_at"] = outcome.ExpiresAt.Format(time.RFC3339)
		bodyResponse["login_option"] = outcome.LoginOption
	}
	return okEnvelope(managementResponse{
		StatusCode: http.StatusOK,
		Headers:    jsonHeaders(),
		Body:       mustJSON(bodyResponse),
	})
}

func processBrowserCallback(callbackURL *url.URL, callbackID string) (browserCallbackOutcome, error) {
	var outcome browserCallbackOutcome
	query := callbackURL.Query()
	state := strings.TrimSpace(query.Get("state"))
	if state == "" {
		return outcome, callbackStatusError(http.StatusBadRequest, "missing_state", "Kiro callback URL has no state")
	}
	session, exists := browserLoginSessionForState(state)
	if !exists || session.LoginState.State != state {
		return outcome, callbackStatusError(http.StatusBadRequest, "unknown_state", "Kiro login state is unknown or expired; start login again")
	}
	expiresAt, errExpiry := time.Parse(time.RFC3339, session.LoginState.ExpiresAt)
	if errExpiry != nil || !time.Now().UTC().Before(expiresAt) {
		clearBrowserLoginSession(state)
		return outcome, callbackStatusError(http.StatusBadRequest, "expired_state", "Kiro login state has expired; start login again")
	}
	if !callbackMatchesRedirect(callbackURL, session.LoginState.RedirectURI) {
		return outcome, callbackStatusError(http.StatusBadRequest, "invalid_callback", "Kiro callback URL does not match the login redirect URI")
	}
	callback := oauthCallbackPayload{
		Code: strings.TrimSpace(query.Get("code")), State: state, Error: strings.TrimSpace(query.Get("error")),
	}
	if callback.Code != "" || callback.Error != "" {
		if !storeBrowserCallback(state, callback) {
			return outcome, callbackStatusError(http.StatusConflict, "login_already_continued", "Kiro login has already continued; return to CPA")
		}
		return browserCallbackOutcome{Status: "accepted", State: state, OAuthError: callback.Error != ""}, nil
	}

	loginOption := normalizeKiroLoginOption(query.Get("login_option"))
	if loginOption == "" {
		loginOption = normalizeKiroLoginOption(query.Get("loginOption"))
	}
	if loginOption == "" {
		return outcome, callbackStatusError(http.StatusBadRequest, "missing_login_option", "Kiro callback has neither an OAuth code nor a supported login option")
	}
	if !claimBrowserDeviceContinuation(state) {
		return outcome, callbackStatusError(http.StatusConflict, "login_already_continued", "Kiro organization login has already continued; open the current verification link")
	}
	claimed := true
	defer func() {
		if claimed {
			releaseBrowserDeviceContinuation(state)
		}
	}()
	startURL := strings.TrimSpace(query.Get("issuer_url"))
	if startURL == "" {
		startURL = strings.TrimSpace(query.Get("issuerUrl"))
	}
	if startURL == "" && loginOption == "builderid" {
		startURL = defaultSSOStartURL
	}
	region := strings.TrimSpace(query.Get("idc_region"))
	if region == "" {
		region = strings.TrimSpace(query.Get("idcRegion"))
	}
	if region == "" {
		region = strings.TrimSpace(loadedConfig().SSORegion)
	}
	if region == "" {
		region = defaultRegion
	}
	apiRegion := jsonx.NonEmpty(strings.TrimSpace(session.LoginState.APIRegion), configuredAPIRegion(loadedConfig()))
	deviceState, verificationURL, expiresAt, errDevice := beginDeviceAuthorization(callbackID, state, startURL, region, apiRegion)
	if errDevice != nil {
		return outcome, errDevice
	}
	if !storeBrowserDeviceContinuation(state, deviceState) {
		clearDeviceLoginPoll(state)
		return outcome, callbackStatusError(http.StatusBadRequest, "unknown_state", "Kiro login state is unknown or expired; start login again")
	}
	claimed = false
	return browserCallbackOutcome{
		Status: "continue", State: state, URL: verificationURL, UserCode: deviceState.UserCode,
		ExpiresAt: expiresAt, LoginOption: loginOption,
	}, nil
}

func validateVerificationURL(raw, region, startURL string) error {
	parsed, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return callbackStatusError(http.StatusBadGateway, "invalid_login_response", "Kiro device authorization returned an unsafe verification URL")
	}
	expectedHost := "device.sso." + strings.ToLower(strings.TrimSpace(region)) + ".amazonaws.com"
	if strings.EqualFold(parsed.Hostname(), expectedHost) {
		return nil
	}
	issuer, errIssuer := url.Parse(strings.TrimSpace(startURL))
	if errIssuer == nil && issuer.Scheme == "https" && issuer.User == nil && strings.EqualFold(parsed.Host, issuer.Host) {
		return nil
	}
	return callbackStatusError(http.StatusBadGateway, "invalid_login_response", "Kiro device authorization returned an untrusted verification URL")
}

func callbackMatchesRedirect(callbackURL *url.URL, redirectURI string) bool {
	expected, errParse := parseBrowserRedirectURI(redirectURI)
	if errParse != nil || callbackURL == nil || callbackURL.User != nil {
		return false
	}
	path := strings.TrimRight(callbackURL.Path, "/")
	return strings.EqualFold(callbackURL.Scheme, expected.Scheme) &&
		strings.EqualFold(callbackURL.Host, expected.Host) &&
		(path == expected.Path || path == expected.Path+"/signin/callback" || path == expected.Path+"/oauth/callback")
}

func parseBrowserRedirectURI(raw string) (*url.URL, error) {
	parsed, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, callbackStatusError(http.StatusBadRequest, "invalid_callback", "Kiro browser redirect URI is invalid")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		host := strings.ToLower(parsed.Hostname())
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			return nil, callbackStatusError(http.StatusBadRequest, "invalid_callback", "Remote Kiro browser redirect URI must use HTTPS")
		}
	default:
		return nil, callbackStatusError(http.StatusBadRequest, "invalid_callback", "Kiro browser redirect URI must use HTTP or HTTPS")
	}
	if strings.HasPrefix(parsed.Path, "/v0/resource/") {
		return nil, callbackStatusError(http.StatusBadRequest, "invalid_callback", "Public resource OAuth callbacks are no longer supported; use a loopback redirect with authenticated callback submission or aws-device login")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func normalizeKiroLoginOption(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "awsidc", "aws-idc", "idc", "sso", "organization", "organisation", "enterprise", "iam", "iam-identity-center":
		return "awsidc"
	case "builderid", "builder-id", "builder_id", "internal":
		return "builderid"
	default:
		return ""
	}
}

func browserCallbackError(status int, code, message string) ([]byte, error) {
	return okEnvelope(managementResponse{
		StatusCode: status,
		Headers:    jsonHeaders(),
		Body:       mustJSON(map[string]any{"error": code, "message": message}),
	})
}

func browserCallbackErrorResponse(err error) ([]byte, error) {
	status := pluginHTTPStatus(err)
	if status == 0 {
		status = http.StatusInternalServerError
	}
	code := "callback_failed"
	message := "Kiro callback failed"
	if typed, ok := err.(statusError); ok {
		code = typed.Code
		message = typed.Error()
	}
	return browserCallbackError(status, code, message)
}

func callbackStatusError(status int, code, message string) statusError {
	return statusError{Code: code, Message: message, HTTPStatus: status}
}
