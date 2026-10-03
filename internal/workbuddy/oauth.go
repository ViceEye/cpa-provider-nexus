package workbuddy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type workbuddyLoginSession struct {
	Region    string
	ExpiresAt time.Time
}

var loginSessions = struct {
	sync.Mutex
	items map[string]workbuddyLoginSession
}{items: make(map[string]workbuddyLoginSession)}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func stringFromMetadata(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	v, ok := metadata[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

// LoginStart starts the CodeBuddy / WorkBuddy browser OAuth flow.
func LoginStart(raw []byte) ([]byte, error) {
	var req struct {
		Provider string         `json:"Provider"`
		Metadata map[string]any `json:"Metadata"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	mode := strings.ToLower(strings.TrimSpace(stringFromMetadata(req.Metadata, "login_mode")))
	reg := strings.ToLower(strings.TrimSpace(stringFromMetadata(req.Metadata, "region")))

	region := RegionCN
	if mode == TypeMarkerIntl || reg == RegionIntl || strings.Contains(mode, "intl") {
		region = RegionIntl
	}

	stateURL := CNStateURL + "?platform=" + CNPlatform
	userAgent := CNUserAgent
	domain := CNDomain
	if region == RegionIntl {
		stateURL = IntlStateURL + "?platform=" + IntlPlatform
		userAgent = IntlUserAgent
		domain = IntlDomain
	}

	startHeaders := http.Header{
		"Content-Type":       []string{"application/json"},
		"Accept":             []string{"application/json"},
		"User-Agent":         []string{userAgent},
		"X-Requested-With":   []string{"XMLHttpRequest"},
		"X-Domain":           []string{domain},
		"X-No-Authorization": []string{"true"},
		"X-No-User-Id":       []string{"true"},
		"X-Product":          []string{"SaaS"},
	}

	resp, err := hostHTTP(hostHTTPRequest{
		Method:  http.MethodPost,
		URL:     stateURL,
		Headers: startHeaders,
		Body:    []byte("{}"),
	})
	if err != nil {
		return errorEnvelope("login_failed", "WorkBuddy state request failed: "+err.Error(), true, http.StatusBadGateway), nil
	}
	if resp.StatusCode != http.StatusOK {
		return errorEnvelope("login_failed", fmt.Sprintf("WorkBuddy state request returned HTTP %d: %s", resp.StatusCode, string(resp.Body)), false, resp.StatusCode), nil
	}

	var stateData struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &stateData); err != nil {
		return errorEnvelope("invalid_response", "Failed to parse WorkBuddy state response: "+err.Error(), false, http.StatusBadGateway), nil
	}
	if stateData.Code != 0 || stateData.Data.State == "" || stateData.Data.AuthURL == "" {
		return errorEnvelope("login_failed", fmt.Sprintf("WorkBuddy state error (%d): %s", stateData.Code, stateData.Msg), false, http.StatusBadRequest), nil
	}

	state := stateData.Data.State
	loginSessions.Lock()
	for key, session := range loginSessions.items {
		if time.Now().UTC().After(session.ExpiresAt) {
			delete(loginSessions.items, key)
		}
	}
	loginSessions.items[state] = workbuddyLoginSession{
		Region:    region,
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}
	loginSessions.Unlock()

	return okEnvelope(map[string]any{
		"Provider":  pluginProvider,
		"URL":       stateData.Data.AuthURL,
		"State":     state,
		"ExpiresAt": time.Now().UTC().Add(15 * time.Minute),
		"Metadata": map[string]any{
			"login_mode": TypeMarker,
			"region":     region,
			"state":      state,
		},
	})
}

// LoginPoll checks whether the user has authorized the login session in the browser.
func LoginPoll(raw []byte) ([]byte, error) {
	var req struct {
		Provider string         `json:"Provider"`
		State    string         `json:"State"`
		Metadata map[string]any `json:"Metadata"`
		Body     map[string]any `json:"Body"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	state := strings.TrimSpace(req.State)
	if state == "" {
		state = strings.TrimSpace(stringFromMetadata(req.Metadata, "state"))
	}

	loginSessions.Lock()
	session, ok := loginSessions.items[state]
	loginSessions.Unlock()

	if !ok || state == "" {
		return okEnvelope(map[string]any{"Status": "error", "Message": "WorkBuddy login session is unknown or expired"})
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		loginSessions.Lock()
		delete(loginSessions.items, state)
		loginSessions.Unlock()
		return okEnvelope(map[string]any{"Status": "error", "Message": "WorkBuddy login session has expired"})
	}

	region := session.Region
	tokenURL := CNTokenURL + "?state=" + url.QueryEscape(state)
	userAgent := CNUserAgent
	domain := CNDomain
	if region == RegionIntl {
		tokenURL = IntlTokenURL + "?state=" + url.QueryEscape(state)
		userAgent = IntlUserAgent
		domain = IntlDomain
	}

	pollHeaders := http.Header{
		"Accept":               []string{"application/json"},
		"User-Agent":           []string{userAgent},
		"X-Requested-With":     []string{"XMLHttpRequest"},
		"X-Domain":             []string{domain},
		"X-No-Authorization":   []string{"true"},
		"X-No-User-Id":         []string{"true"},
		"X-No-Enterprise-Id":   []string{"true"},
		"X-No-Department-Info": []string{"true"},
		"X-Product":            []string{"SaaS"},
	}

	resp, err := hostHTTP(hostHTTPRequest{
		Method:  http.MethodGet,
		URL:     tokenURL,
		Headers: pollHeaders,
	})
	if err != nil {
		return okEnvelope(map[string]any{"Status": "error", "Message": "WorkBuddy token poll failed: " + err.Error()})
	}
	if resp.StatusCode != http.StatusOK {
		return okEnvelope(map[string]any{"Status": "error", "Message": fmt.Sprintf("WorkBuddy token poll HTTP %d", resp.StatusCode)})
	}

	var tokenData struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			TokenType    string `json:"tokenType"`
			ExpiresIn    int    `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &tokenData); err != nil {
		return okEnvelope(map[string]any{"Status": "error", "Message": "Failed to parse WorkBuddy token poll response: " + err.Error()})
	}

	// 11217 = pending authorization
	if tokenData.Code == 11217 {
		return okEnvelope(map[string]any{
			"Status":  "pending",
			"Message": "Waiting for WorkBuddy authorization in browser",
		})
	}

	if tokenData.Code == 0 && tokenData.Data.AccessToken != "" {
		expiresIn := tokenData.Data.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 86400
		}

		cred := credential{
			Type:          pluginProvider,
			Kind:          TypeMarker,
			Region:        region,
			Version:       1,
			AuthID:        "workbuddy-" + randomID()[:12],
			AccessToken:   tokenData.Data.AccessToken,
			RefreshToken:  tokenData.Data.RefreshToken,
			ExpiresAt:     time.Now().UTC().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339),
			LastRefreshAt: time.Now().UTC().Format(time.RFC3339),
		}

		loginSessions.Lock()
		delete(loginSessions.items, state)
		loginSessions.Unlock()

		auth, err := authDataFromCredential(cred)
		if err != nil {
			return okEnvelope(map[string]any{"Status": "error", "Message": err.Error()})
		}

		return okEnvelope(map[string]any{
			"Status":  "success",
			"Message": "WorkBuddy login completed",
			"Auth":    auth,
		})
	}

	errMsg := tokenData.Msg
	if errMsg == "" {
		errMsg = fmt.Sprintf("upstream error code %d", tokenData.Code)
	}
	return okEnvelope(map[string]any{"Status": "error", "Message": "WorkBuddy login failed: " + errMsg})
}
