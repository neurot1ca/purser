package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const catalogURL = "https://models.dev/api.json"

var modelsDevURL = catalogURL

// RefreshCatalog refreshes the checked models.dev cache when its last valid
// fetch is at least 24 hours old.
func RefreshCatalog(ctx context.Context, stateRoot string, now time.Time) error {
	path := filepath.Join(stateRoot, "model-catalog", "catalog.json")
	if fetchedAt, _, err := readCatalogCache(path); err == nil {
		if now.Sub(fetchedAt) < time.Hour {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "purser")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("models.dev returned HTTP %d", response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogBytes+1))
	if err != nil {
		return err
	}
	if len(payload) > maxCatalogBytes {
		return errors.New("models.dev response exceeds size limit")
	}
	providers, dropped, err := normalizeCatalog(payload)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cacheProviders := make(map[string]any, len(providers))
	for providerID, models := range providers {
		cacheProviders[providerID] = map[string]any{"models": models}
	}
	utcNow := now.UTC().Truncate(time.Microsecond)
	fetchedAt := utcNow.Format("2006-01-02T15:04:05")
	if utcNow.Nanosecond() != 0 {
		fetchedAt = utcNow.Format("2006-01-02T15:04:05.000000")
	}
	fetchedAt += "Z"
	envelope := map[string]any{
		"fetched_at":    fetchedAt,
		"dropped_count": dropped,
		"providers":     cacheProviders,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return atomicCatalogWrite(path, encoded)
}

func normalizeCatalog(payload []byte) (map[string]map[string]any, int, error) {
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, 0, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, 0, errors.New("catalog must be a provider object")
	}
	providers := make(map[string]map[string]any)
	dropped := 0
	validModels := 0
	for providerID, providerValue := range root {
		provider, ok := providerValue.(map[string]any)
		if !ok || providerID == "" {
			dropped++
			continue
		}
		models, ok := provider["models"].(map[string]any)
		if !ok {
			dropped++
			continue
		}
		checked := make(map[string]any)
		for modelID, modelValue := range models {
			fact, valid := normalizeModel(modelID, modelValue)
			if !valid {
				dropped++
				continue
			}
			checked[modelID] = fact
			validModels++
		}
		providers[providerID] = checked
	}
	if validModels == 0 {
		return nil, dropped, errors.New("catalog contains no valid models")
	}
	return providers, dropped, nil
}

func normalizeModel(modelID string, value any) (map[string]any, bool) {
	model, ok := value.(map[string]any)
	if !ok || modelID == "" {
		return nil, false
	}
	name, nameOK := model["name"].(string)
	reasoning, reasoningOK := model["reasoning"].(bool)
	if !nameOK || name == "" || !reasoningOK {
		return nil, false
	}
	fact := map[string]any{"id": modelID, "name": name, "reasoning": reasoning}
	if releaseDate, ok := model["release_date"].(string); ok && releaseDate != "" {
		fact["release_date"] = releaseDate
	}
	if cost, ok := model["cost"].(map[string]any); ok && validCost(cost) {
		fact["cost"] = map[string]any{"input": cost["input"], "output": cost["output"]}
	}
	if familyValue, exists := model["family"]; exists {
		family, ok := familyValue.(string)
		if !ok || family == "" {
			return nil, false
		}
		fact["family"] = family
	}
	limitsValue, exists := model["limit"]
	if !exists {
		limitsValue, exists = model["limits"]
	}
	limits := map[string]any{}
	if exists {
		values, ok := limitsValue.(map[string]any)
		if !ok {
			return nil, false
		}
		for _, key := range []string{"context", "output"} {
			if raw, present := values[key]; present {
				count, ok := positiveInteger(raw)
				if !ok {
					return nil, false
				}
				limits[key] = count
			}
		}
	}
	fact["limits"] = limits
	return fact, true
}

func validCost(cost map[string]any) bool {
	for _, key := range []string{"input", "output"} {
		number, ok := cost[key].(json.Number)
		if !ok {
			return false
		}
		value, err := number.Float64()
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
			return false
		}
	}
	return true
}

func positiveInteger(value any) (any, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, false
	}
	integer, valid := new(big.Int).SetString(string(number), 10)
	if !valid || integer.Sign() <= 0 || strings.ContainsAny(string(number), ".eE") {
		return nil, false
	}
	return number, true
}

func readCatalogCache(path string) (time.Time, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, 0, err
	}
	if len(data) > maxCatalogBytes {
		return time.Time{}, 0, errors.New("invalid model catalog cache")
	}
	var envelope struct {
		FetchedAt string          `json:"fetched_at"`
		Providers json.RawMessage `json:"providers"`
		Dropped   json.RawMessage `json:"dropped_count"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.FetchedAt == "" {
		return time.Time{}, 0, errors.New("invalid model catalog cache")
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, envelope.FetchedAt)
	if err != nil {
		return time.Time{}, 0, err
	}
	var dropped int64
	if len(envelope.Dropped) > 0 && (string(envelope.Dropped) == "null" || json.Unmarshal(envelope.Dropped, &dropped) != nil || dropped < 0) {
		return time.Time{}, 0, errors.New("invalid model catalog dropped count")
	}
	if _, _, err := normalizeCatalog(envelope.Providers); err != nil {
		return time.Time{}, 0, err
	}
	return fetchedAt, dropped, nil
}

func atomicCatalogWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tempName := temporary.Name()
	defer os.Remove(tempName)
	if _, err = temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err = temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}
