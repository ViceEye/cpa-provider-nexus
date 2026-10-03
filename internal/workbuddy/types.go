package workbuddy

import (
	"net/http"
	"time"
)

const (
	TypeMarker     = "workbuddy"
	TypeMarkerCN   = "workbuddy-cn"
	TypeMarkerIntl = "workbuddy-intl"
	pluginProvider = "nexus"

	RegionCN   = "cn"
	RegionIntl = "intl"

	// 国内版 (CodeBuddy CN / WorkBuddy)
	CNBaseURL    = "https://copilot.tencent.com"
	CNChatPath   = "/v2/chat/completions"
	CNStateURL   = "https://copilot.tencent.com/v2/plugin/auth/state"
	CNTokenURL   = "https://copilot.tencent.com/v2/plugin/auth/token"
	CNRefreshURL = "https://copilot.tencent.com/v2/plugin/auth/token/refresh"
	CNUsageURL   = "https://copilot.tencent.com/v2/billing/meter/get-user-resource"
	CNPlatform   = "CLI"
	CNUserAgent  = "CLI/2.108.1 CodeBuddy/2.108.1"
	CNDomain     = "copilot.tencent.com"

	// 国际版 (CodeBuddy Intl)
	IntlBaseURL    = "https://www.codebuddy.ai"
	IntlChatPath   = "/v2/chat/completions"
	IntlStateURL   = "https://www.codebuddy.ai/v2/plugin/auth/state"
	IntlTokenURL   = "https://www.codebuddy.ai/v2/plugin/auth/token"
	IntlRefreshURL = "https://www.codebuddy.ai/v2/plugin/auth/token/refresh"
	IntlUsageURL   = "https://www.codebuddy.ai/v2/billing/meter/get-user-resource"
	IntlPlatform   = "ide"
	IntlUserAgent  = "IDE/2.108.1 CodeBuddy/2.108.1"
	IntlDomain     = "www.codebuddy.ai"

	modelPrefix = "nexus/"
)

// credential is the plugin's stored credential JSON (StorageJSON).
type credential struct {
	originalJSON  []byte
	Type          string `json:"type"`
	Kind          string `json:"kind,omitempty"`
	Region        string `json:"region,omitempty"`
	Version       int    `json:"version"`
	AuthID        string `json:"auth_id,omitempty"`
	AccountName   string `json:"account_name,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	APIKey        string `json:"api_key,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	LastRefreshAt string `json:"last_refresh_at,omitempty"`
}

type authData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName"`
	Label            string            `json:"Label"`
	StorageJSON      []byte            `json:"StorageJSON"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter,omitempty"`
}

type modelInfo struct {
	ID          string `json:"ID"`
	Object      string `json:"Object"`
	OwnedBy     string `json:"OwnedBy"`
	DisplayName string `json:"DisplayName"`
	Type        string `json:"Type,omitempty"`
}

type statusError struct {
	Code       string
	Message    string
	Retryable  bool
	HTTPStatus int
}

func (e statusError) Error() string { return e.Message }

func statusErr(code, message string, retryable bool, status int) statusError {
	return statusError{Code: code, Message: message, Retryable: retryable, HTTPStatus: status}
}

type hostConfigSummary struct {
	AuthDir string `json:"AuthDir"`
}

type authParseRequest struct {
	Provider string            `json:"Provider"`
	Path     string            `json:"Path"`
	FileName string            `json:"FileName"`
	RawJSON  []byte            `json:"RawJSON"`
	Host     hostConfigSummary `json:"Host"`
}

type authParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    authData   `json:"Auth,omitempty"`
	Auths   []authData `json:"Auths,omitempty"`
}

type authRefreshRequest struct {
	AuthID         string            `json:"AuthID"`
	AuthProvider   string            `json:"AuthProvider"`
	StorageJSON    []byte            `json:"StorageJSON"`
	Metadata       map[string]any    `json:"Metadata"`
	Attributes     map[string]string `json:"Attributes"`
	Host           hostConfigSummary `json:"Host"`
	HostCallbackID string            `json:"host_callback_id,omitempty"`
}

type authRefreshResponse struct {
	Auth             authData  `json:"Auth"`
	NextRefreshAfter time.Time `json:"NextRefreshAfter"`
}

type authModelRequest struct {
	AuthID         string            `json:"AuthID"`
	AuthProvider   string            `json:"AuthProvider"`
	StorageJSON    []byte            `json:"StorageJSON"`
	Metadata       map[string]any    `json:"Metadata"`
	Attributes     map[string]string `json:"Attributes"`
	Host           hostConfigSummary `json:"Host"`
	HostCallbackID string            `json:"host_callback_id,omitempty"`
}

type modelResponse struct {
	Provider   string      `json:"Provider"`
	Models     []modelInfo `json:"Models"`
	AuthUpdate authData    `json:"AuthUpdate,omitempty"`
}

type executorRequest struct {
	AuthID         string      `json:"AuthID"`
	AuthProvider   string      `json:"AuthProvider"`
	Model          string      `json:"Model"`
	Format         string      `json:"Format"`
	Stream         bool        `json:"Stream"`
	Headers        http.Header `json:"Headers"`
	SourceFormat   string      `json:"SourceFormat"`
	Payload        []byte      `json:"Payload"`
	StorageJSON    []byte      `json:"StorageJSON"`
	StreamID       string      `json:"stream_id,omitempty"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type executorResponse struct {
	Payload  []byte         `json:"Payload"`
	Headers  http.Header    `json:"Headers,omitempty"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

type managementRequest struct {
	Method         string              `json:"Method"`
	Path           string              `json:"Path"`
	Headers        http.Header         `json:"Headers"`
	Query          map[string][]string `json:"Query"`
	Body           []byte              `json:"Body"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

func (r managementRequest) QueryValue(key string) string {
	if r.Query == nil {
		return ""
	}
	values := r.Query[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

// 国内版模型列表 (对应 9router codebuddy-cn)
var cnModels = []modelInfo{
	{ID: "glm-5.3", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5.3"},
	{ID: "glm-5.3-flash", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5.3 Flash"},
	{ID: "glm-5.2", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5.2"},
	{ID: "glm-5.1", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5.1"},
	{ID: "glm-5v-turbo", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5v-Turbo"},
	{ID: "kimi-k3-1", Object: "model", OwnedBy: "moonshot", DisplayName: "Kimi K3"},
	{ID: "kimi-k2.7", Object: "model", OwnedBy: "moonshot", DisplayName: "Kimi K2.7 Code"},
	{ID: "kimi-k2.6", Object: "model", OwnedBy: "moonshot", DisplayName: "Kimi K2.6"},
	{ID: "deepseek-v4-pro", Object: "model", OwnedBy: "deepseek", DisplayName: "DeepSeek V4 Pro"},
	{ID: "deepseek-v4.1-flash", Object: "model", OwnedBy: "deepseek", DisplayName: "DeepSeek V4.1 Flash"},
	{ID: "hy3", Object: "model", OwnedBy: "tencent", DisplayName: "Hunyuan 3"},
	{ID: "hy4-preview", Object: "model", OwnedBy: "tencent", DisplayName: "Hunyuan 4 Preview"},
	{ID: "minimax-m3", Object: "model", OwnedBy: "minimax", DisplayName: "MiniMax M3"},
}

// 国际版模型列表 (对应 9router codebuddy-intl)
var intlModels = []modelInfo{
	{ID: "glm-5.2", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5.2"},
	{ID: "glm-5.1", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5.1"},
	{ID: "glm-5.0", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5.0"},
	{ID: "glm-5.0-turbo", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5.0 Turbo"},
	{ID: "glm-5v-turbo", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-5v-Turbo"},
	{ID: "glm-4.7", Object: "model", OwnedBy: "z-ai", DisplayName: "GLM-4.7"},
	{ID: "minimax-m3", Object: "model", OwnedBy: "minimax", DisplayName: "MiniMax M3"},
	{ID: "minimax-m2.7", Object: "model", OwnedBy: "minimax", DisplayName: "MiniMax M2.7"},
	{ID: "kimi-k2.7", Object: "model", OwnedBy: "moonshot", DisplayName: "Kimi K2.7 Code"},
	{ID: "kimi-k2.6", Object: "model", OwnedBy: "moonshot", DisplayName: "Kimi K2.6"},
	{ID: "kimi-k2.5", Object: "model", OwnedBy: "moonshot", DisplayName: "Kimi K2.5"},
	{ID: "hy3-preview", Object: "model", OwnedBy: "tencent", DisplayName: "Hunyuan 3 Preview"},
	{ID: "deepseek-v4-pro", Object: "model", OwnedBy: "deepseek", DisplayName: "DeepSeek V4 Pro"},
	{ID: "deepseek-v4.1-flash", Object: "model", OwnedBy: "deepseek", DisplayName: "DeepSeek V4.1 Flash"},
	{ID: "deepseek-v3-2-volc", Object: "model", OwnedBy: "deepseek", DisplayName: "DeepSeek V3.2"},
}
