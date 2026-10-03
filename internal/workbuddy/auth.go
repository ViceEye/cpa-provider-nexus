package workbuddy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// buildWorkBuddyHeaders constructs the HTTP headers expected by CodeBuddy/WorkBuddy gateways.
func buildWorkBuddyHeaders(cred credential, isStream bool) http.Header {
	h := make(http.Header)
	token := cred.AccessToken
	if token == "" {
		token = cred.APIKey
	}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}

	region := strings.ToLower(strings.TrimSpace(cred.Region))
	if region == RegionIntl {
		h.Set("User-Agent", IntlUserAgent)
		h.Set("X-Product", "SaaS")
		h.Set("X-IDE-Type", "IDE")
		h.Set("X-IDE-Name", "IDE")
		h.Set("X-Domain", IntlDomain)
		h.Set("x-codebuddy-request", "1")
		h.Set("x-requested-with", "XMLHttpRequest")
	} else {
		// default to CN
		h.Set("User-Agent", CNUserAgent)
		h.Set("X-Product", "SaaS")
		h.Set("X-IDE-Type", "CLI")
		h.Set("X-IDE-Name", "CLI")
		h.Set("X-Domain", CNDomain)
		h.Set("x-codebuddy-request", "1")
		h.Set("x-requested-with", "XMLHttpRequest")
	}

	h.Set("Content-Type", "application/json")
	if isStream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	return h
}

func decodeCredential(raw []byte) (credential, error) {
	var cred credential
	if len(raw) == 0 {
		return cred, fmt.Errorf("WorkBuddy auth storage is empty")
	}
	if err := json.Unmarshal(raw, &cred); err != nil {
		return cred, fmt.Errorf("decode WorkBuddy auth storage: %w", err)
	}
	if cred.Type != pluginProvider || (cred.Kind != TypeMarker && cred.Kind != TypeMarkerCN && cred.Kind != TypeMarkerIntl) {
		return cred, fmt.Errorf("not a Nexus WorkBuddy credential")
	}
	cred.Region = strings.ToLower(strings.TrimSpace(cred.Region))
	if cred.Region == "" {
		cred.Region = RegionCN
		if cred.Kind == TypeMarkerIntl {
			cred.Region = RegionIntl
		}
	}
	if cred.Region != RegionCN && cred.Region != RegionIntl {
		return cred, fmt.Errorf("invalid WorkBuddy region")
	}
	cred.Kind = TypeMarker
	cred.AccessToken = strings.TrimSpace(cred.AccessToken)
	cred.APIKey = strings.TrimSpace(cred.APIKey)
	cred.RefreshToken = strings.TrimSpace(cred.RefreshToken)
	if cred.Version == 0 {
		cred.Version = 1
	}
	if cred.AccessToken == "" && cred.APIKey == "" && cred.RefreshToken == "" {
		return cred, fmt.Errorf("WorkBuddy credential has neither access_token nor api_key")
	}
	if cred.AuthID == "" {
		cred.AuthID = credentialID(cred)
	}
	cred.originalJSON = append([]byte(nil), raw...)
	return cred, nil
}

func credentialNeedsRefresh(cred credential) bool {
	// API key credentials without refresh token never refresh
	if strings.TrimSpace(cred.APIKey) != "" && strings.TrimSpace(cred.RefreshToken) == "" {
		return false
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return true
	}
	expires, err := time.Parse(time.RFC3339, cred.ExpiresAt)
	if err != nil {
		return false // no expiry known, rely on 401 retry
	}
	return expires.Before(time.Now().UTC().Add(5 * time.Minute))
}

func credentialID(cred credential) string {
	if strings.HasPrefix(cred.AuthID, "workbuddy-") && !strings.ContainsAny(cred.AuthID, "/\\.") {
		return cred.AuthID
	}
	region := cred.Region
	if region == "" {
		region = RegionCN
	}
	tokenIdentity := cred.AccessToken
	if tokenIdentity == "" {
		tokenIdentity = cred.APIKey
	}
	identity := region + "\x00" + cred.AccountName + "\x00" + cred.RefreshToken + "\x00" + tokenIdentity
	sum := sha256.Sum256([]byte(identity))
	return "workbuddy-" + hex.EncodeToString(sum[:10])
}

// StableCredentialID returns the internal credential ID for stats and usage.
func StableCredentialID(raw []byte) string {
	cred, err := decodeCredential(raw)
	if err != nil {
		return ""
	}
	if id := strings.TrimSpace(cred.AuthID); id != "" {
		return id
	}
	if strings.TrimSpace(cred.AccountName) == "" && strings.TrimSpace(cred.RefreshToken) == "" && strings.TrimSpace(cred.AccessToken) == "" && strings.TrimSpace(cred.APIKey) == "" {
		return ""
	}
	return credentialID(cred)
}

func authDataFromCredential(cred credential) (authData, error) {
	if strings.TrimSpace(cred.AccessToken) == "" && strings.TrimSpace(cred.APIKey) == "" && strings.TrimSpace(cred.RefreshToken) == "" {
		return authData{}, fmt.Errorf("WorkBuddy credential has no access token or api key")
	}
	cred.AuthID = credentialID(cred)
	storageJSON, err := json.Marshal(cred)
	if err != nil {
		return authData{}, fmt.Errorf("encode WorkBuddy credential: %w", err)
	}
	// Preserve host-owned fields such as disabled, priority and note on refresh.
	if len(cred.originalJSON) > 0 {
		var original, fresh map[string]json.RawMessage
		if err := json.Unmarshal(cred.originalJSON, &original); err != nil {
			return authData{}, err
		}
		if err := json.Unmarshal(storageJSON, &fresh); err != nil {
			return authData{}, err
		}
		for key, value := range fresh {
			original[key] = value
		}
		storageJSON, err = json.Marshal(original)
		if err != nil {
			return authData{}, err
		}
	}

	label := "WorkBuddy"
	region := strings.ToLower(strings.TrimSpace(cred.Region))
	if region == RegionIntl {
		label = "WorkBuddy (Intl)"
	} else {
		label = "WorkBuddy (CN)"
	}
	if cred.AccountName != "" {
		label = fmt.Sprintf("%s (%s)", label, cred.AccountName)
	}

	var nextRefresh time.Time
	if cred.ExpiresAt != "" {
		if exp, err := time.Parse(time.RFC3339, cred.ExpiresAt); err == nil {
			nextRefresh = exp.Add(-5 * time.Minute)
		}
	}

	return authData{
		Provider:         pluginProvider,
		ID:               credentialID(cred),
		FileName:         credentialID(cred) + ".json",
		Label:            label,
		StorageJSON:      storageJSON,
		NextRefreshAfter: nextRefresh,
		Attributes: map[string]string{
			"region":       region,
			"account_name": cred.AccountName,
		},
	}, nil
}

func refreshCredential(cred credential, hostCallbackID string) (credential, error) {
	if strings.TrimSpace(cred.RefreshToken) == "" {
		if strings.TrimSpace(cred.APIKey) != "" {
			return cred, nil
		}
		return cred, statusErr("missing_refresh_token", "WorkBuddy credential has no refresh token", false, http.StatusUnauthorized)
	}

	region := strings.ToLower(strings.TrimSpace(cred.Region))
	refreshURL := CNRefreshURL
	domain := CNDomain
	userAgent := CNUserAgent
	if region == RegionIntl {
		refreshURL = IntlRefreshURL
		domain = IntlDomain
		userAgent = IntlUserAgent
	}

	reqHeaders := http.Header{
		"Content-Type":          []string{"application/json"},
		"Accept":                []string{"application/json"},
		"User-Agent":            []string{userAgent},
		"X-Requested-With":      []string{"XMLHttpRequest"},
		"X-Domain":              []string{domain},
		"X-Refresh-Token":       []string{cred.RefreshToken},
		"X-Auth-Refresh-Source": []string{"plugin"},
		"X-Product":             []string{"SaaS"},
	}

	resp, err := hostHTTP(hostHTTPRequest{
		HostCallbackID: hostCallbackID,
		Method:         http.MethodPost,
		URL:            refreshURL,
		Headers:        reqHeaders,
		Body:           []byte("{}"),
	})
	if err != nil {
		return cred, statusErr("refresh_failed", "WorkBuddy token refresh request failed: "+err.Error(), true, http.StatusBadGateway)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return cred, statusErr("refresh_token_revoked", "WorkBuddy refresh token rejected by upstream", false, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return cred, statusErr("refresh_failed", fmt.Sprintf("WorkBuddy refresh failed with HTTP %d", resp.StatusCode), true, resp.StatusCode)
	}

	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int    `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return cred, statusErr("invalid_refresh_response", "cannot decode WorkBuddy refresh response: "+err.Error(), false, http.StatusBadGateway)
	}
	if data.Code != 0 || data.Data.AccessToken == "" {
		errMsg := data.Msg
		if errMsg == "" {
			errMsg = "missing accessToken"
		}
		return cred, statusErr("refresh_failed", fmt.Sprintf("WorkBuddy refresh error (code %d): %s", data.Code, errMsg), false, http.StatusUnauthorized)
	}

	cred.AccessToken = data.Data.AccessToken
	if data.Data.RefreshToken != "" {
		cred.RefreshToken = data.Data.RefreshToken
	}
	expiresIn := data.Data.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 86400
	}
	cred.ExpiresAt = time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
	cred.LastRefreshAt = time.Now().UTC().Format(time.RFC3339)

	return cred, nil
}

func persistCredentialBestEffort(name string, cred credential) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || filepath.IsAbs(name) || strings.Contains(name, "..") {
		return
	}
	auth, err := authDataFromCredential(cred)
	if err != nil {
		return
	}
	_, _ = callHost("host.auth.save", map[string]any{
		"name": name,
		"json": json.RawMessage(auth.StorageJSON),
	})
}

// ParseAuth handles "auth.parse" for workbuddy credentials.
func ParseAuth(raw []byte) ([]byte, error) {
	var req authParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Provider != "" && !strings.EqualFold(req.Provider, pluginProvider) {
		return okEnvelope(authParseResponse{Handled: false})
	}
	cred, err := decodeCredential(req.RawJSON)
	if err != nil {
		return okEnvelope(authParseResponse{Handled: false})
	}
	auth, err := authDataFromCredential(cred)
	if err != nil {
		return nil, err
	}

	physicalName := strings.TrimSpace(req.FileName)
	if physicalName == "" && strings.TrimSpace(req.Path) != "" {
		physicalName = filepath.Base(strings.TrimSpace(req.Path))
	}
	// Adhere to invariant: single-account files take host's file-based record ID.
	auth.ID = ""
	auth.FileName = physicalName

	return okEnvelope(authParseResponse{
		Handled: true,
		Auth:    auth,
		Auths:   []authData{auth},
	})
}

// RefreshAuth handles "auth.refresh" for workbuddy credentials.
func RefreshAuth(raw []byte) ([]byte, error) {
	var req authRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cred, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_auth", err.Error(), false, http.StatusUnauthorized), nil
	}
	refreshed, err := refreshCredential(cred, req.HostCallbackID)
	if err != nil {
		return pluginError(err), nil
	}
	auth, err := authDataFromCredential(refreshed)
	if err != nil {
		return nil, err
	}
	// Empty ID/FileName preserves host record identity.
	auth.ID = ""
	auth.FileName = ""
	for key, value := range req.Attributes {
		auth.Attributes[key] = value
	}
	auth.Metadata = req.Metadata

	return okEnvelope(authRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: auth.NextRefreshAfter,
	})
}
