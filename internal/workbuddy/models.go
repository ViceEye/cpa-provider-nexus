package workbuddy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ModelsForAuth returns the available models for the given WorkBuddy connection.
func ModelsForAuth(raw []byte) ([]byte, error) {
	var req authModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	cred, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return errorEnvelope("invalid_auth", err.Error(), false, http.StatusUnauthorized), nil
	}
	region := RegionCN
	if err == nil && strings.ToLower(strings.TrimSpace(cred.Region)) == RegionIntl {
		region = RegionIntl
	}

	catalog := cnModels
	if region == RegionIntl {
		catalog = intlModels
	}

	models := make([]modelInfo, 0, len(catalog)*2)
	for _, model := range catalog {
		// primary alias: nexus/{modelID}
		entry := model
		entry.ID = modelPrefix + model.ID
		models = append(models, entry)

		// short alias: nexus/wb/{modelID}
		shortEntry := model
		shortEntry.ID = modelPrefix + "wb/" + model.ID
		shortEntry.DisplayName = model.DisplayName + " (wb)"
		models = append(models, shortEntry)
	}

	return okEnvelope(modelResponse{
		Provider: pluginProvider,
		Models:   models,
	})
}
