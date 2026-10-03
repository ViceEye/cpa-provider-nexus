package workbuddy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type billingAccount struct {
	PackageName              string `json:"PackageName"`
	Capacity                 any    `json:"Capacity"`
	PreciseCapacity          string `json:"PreciseCapacity"`
	CapacityUsed             any    `json:"CapacityUsed"`
	PreciseCapacityUsed      string `json:"PreciseCapacityUsed"`
	CycleCapacity            any    `json:"CycleCapacity"`
	PreciseCycleCapacity     string `json:"PreciseCycleCapacity"`
	CycleCapacityUsed        any    `json:"CycleCapacityUsed"`
	PreciseCycleCapacityUsed string `json:"PreciseCycleCapacityUsed"`
	CycleStartTime           string `json:"CycleStartTime"`
	CycleEndTime             string `json:"CycleEndTime"`
	DeductionEndTime         any    `json:"DeductionEndTime"`
}

func parseNumber(precise string, plain any) float64 {
	if precise != "" {
		if v, err := strconv.ParseFloat(precise, 64); err == nil {
			return v
		}
	}
	if plain != nil {
		switch v := plain.(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

func parseUnixSeconds(v any) int64 {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return int64(val)
	case int64:
		return val
	case int:
		return int64(val)
	case string:
		if sec, err := strconv.ParseInt(val, 10, 64); err == nil {
			return sec
		}
	}
	return 0
}

func parseTime(str string) time.Time {
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z",
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, str); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Usage queries the CodeBuddy / WorkBuddy quota endpoint.
func Usage(raw []byte) ([]byte, error) {
	var req managementRequest
	_ = json.Unmarshal(raw, &req)
	callbackID := req.HostCallbackID

	conn := struct {
		StorageJSON    []byte `json:"StorageJSON"`
		AuthIndex      string `json:"auth_index"`
		Name           string `json:"name"`
		HostCallbackID string `json:"host_callback_id"`
	}{}
	_ = json.Unmarshal(req.Body, &conn)
	if callbackID == "" {
		callbackID = conn.HostCallbackID
	}

	cred, err := decodeCredential(conn.StorageJSON)
	if err != nil {
		return managementJSON(http.StatusOK, map[string]any{
			"accounts": []map[string]any{{"status": "error", "error": err.Error()}},
		}), nil
	}

	if credentialNeedsRefresh(cred) {
		refreshed, err := refreshCredential(cred, callbackID)
		if err != nil {
			return managementJSON(http.StatusOK, map[string]any{
				"accounts": []map[string]any{{"status": "error", "error": err.Error()}},
			}), nil
		}
		cred = refreshed
		persistCredentialBestEffort(conn.Name, cred)
	}

	usageURL := CNUsageURL
	if strings.ToLower(strings.TrimSpace(cred.Region)) == RegionIntl {
		usageURL = IntlUsageURL
	}

	headers := buildWorkBuddyHeaders(cred, false)
	resp, err := hostHTTP(hostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         http.MethodPost,
		URL:            usageURL,
		Headers:        headers,
		Body:           []byte("{}"),
	})
	if err != nil {
		return managementJSON(http.StatusOK, map[string]any{
			"accounts": []map[string]any{{"status": "error", "error": "WorkBuddy usage request failed: " + err.Error()}},
		}), nil
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		refreshed, errRefresh := refreshCredential(cred, callbackID)
		if errRefresh != nil {
			return managementJSON(http.StatusOK, map[string]any{"accounts": []map[string]any{{"status": "error", "error": errRefresh.Error()}}}), nil
		}
		cred = refreshed
		persistCredentialBestEffort(conn.Name, cred)
		resp, err = hostHTTP(hostHTTPRequest{HostCallbackID: callbackID, Method: http.MethodPost, URL: usageURL, Headers: buildWorkBuddyHeaders(cred, false), Body: []byte("{}")})
		if err != nil {
			return managementJSON(http.StatusOK, map[string]any{"accounts": []map[string]any{{"status": "error", "error": "WorkBuddy usage retry failed"}}}), nil
		}
	}
	if resp.StatusCode != http.StatusOK {
		return managementJSON(http.StatusOK, map[string]any{
			"accounts": []map[string]any{{"status": "error", "error": fmt.Sprintf("WorkBuddy usage API HTTP %d", resp.StatusCode)}},
		}), nil
	}

	var quotaResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []billingAccount `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}

	if err := json.Unmarshal(resp.Body, &quotaResp); err != nil {
		return managementJSON(http.StatusOK, map[string]any{
			"accounts": []map[string]any{{"status": "error", "error": "Failed to parse usage response: " + err.Error()}},
		}), nil
	}

	if quotaResp.Code != 0 {
		return managementJSON(http.StatusOK, map[string]any{
			"accounts": []map[string]any{{"status": "error", "error": fmt.Sprintf("WorkBuddy quota error (%d): %s", quotaResp.Code, quotaResp.Msg)}},
		}), nil
	}

	accounts := quotaResp.Data.Response.Data.Accounts
	if len(accounts) == 0 {
		return managementJSON(http.StatusOK, map[string]any{
			"accounts": []map[string]any{{
				"status":  "ok",
				"name":    cred.AccountName,
				"plan":    "WorkBuddy",
				"message": "Connected. No active credit package found.",
			}},
		}), nil
	}

	var quotas []map[string]any
	var usage []map[string]any
	var totalRemaining float64

	const refillGapSeconds = 2 * 86400 // 2 days

	for i, acc := range accounts {
		cycleEnd := parseTime(acc.CycleEndTime)
		deductionEndSec := parseUnixSeconds(acc.DeductionEndTime)
		cycleEndSec := cycleEnd.Unix()

		isRefill := cycleEndSec > 0 && deductionEndSec > 0 && (deductionEndSec-cycleEndSec) > refillGapSeconds

		var capVal, usedVal float64
		name := acc.PackageName
		if name == "" {
			name = fmt.Sprintf("Pack %d", i+1)
		}

		if isRefill {
			capVal = parseNumber(acc.PreciseCycleCapacity, acc.CycleCapacity)
			usedVal = parseNumber(acc.PreciseCycleCapacityUsed, acc.CycleCapacityUsed)
		} else {
			capVal = parseNumber(acc.PreciseCapacity, acc.Capacity)
			usedVal = parseNumber(acc.PreciseCapacityUsed, acc.CapacityUsed)
		}

		remaining := capVal - usedVal
		if remaining < 0 {
			remaining = 0
		}
		totalRemaining += remaining

		quotaItem := map[string]any{
			"name":      name,
			"remaining": remaining,
			"total":     capVal,
			"used":      usedVal,
			"unit":      "credit",
			"unlimited": false,
		}
		if !cycleEnd.IsZero() {
			quotaItem["reset_at"] = cycleEnd.Format(time.RFC3339)
		}
		quotas = append(quotas, quotaItem)
		percent := float64(0)
		if capVal > 0 {
			percent = usedVal / capVal * 100
		}
		usage = append(usage, map[string]any{
			"resource_type": "credits", "display_name": name,
			"current_usage": usedVal, "usage_limit": capVal,
			"remaining": remaining, "usage_percent": percent, "unit": "credit",
		})
	}

	accountName := cred.AccountName
	if accountName == "" {
		if strings.ToLower(strings.TrimSpace(cred.Region)) == RegionIntl {
			accountName = "WorkBuddy (Intl)"
		} else {
			accountName = "WorkBuddy (CN)"
		}
	}

	return managementJSON(http.StatusOK, map[string]any{
		"accounts": []map[string]any{{
			"status":       "ok",
			"name":         accountName,
			"plan":         "WorkBuddy",
			"subscription": "WorkBuddy",
			"balance":      totalRemaining,
			"unit":         "credit",
			"quotas":       quotas,
			"usage":        usage,
			"raw_data":     accounts,
		}},
	}), nil
}
