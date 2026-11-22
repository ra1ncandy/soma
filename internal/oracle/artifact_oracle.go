package oracle

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ArtifactOracleSpec validates existence, hash freshness against stale runs, required JSON keys, and metrics.
type ArtifactOracleSpec struct {
	RequiredPaths    []string            `json:"required_paths"`
	DisallowedHashes map[string][]string `json:"disallowed_hashes,omitempty"`
	RequiredJSONKeys map[string][]string `json:"required_json_keys,omitempty"`
	MetricsFile      string              `json:"metrics_file,omitempty"`
	MetricThresholds []MetricThreshold   `json:"metric_thresholds,omitempty"`
}

func evalArtifactOracle(wsDir string, spec ArtifactOracleSpec) (bool, string, string, error) {
	if len(spec.RequiredPaths) == 0 {
		return false, "", "artifact_oracle: no required_paths configured", nil
	}

	var firstSHA string
	var firstJSON []byte

	for _, rel := range spec.RequiredPaths {
		full, err := ResolveSafePath(wsDir, rel)
		if err != nil {
			return false, firstSHA, err.Error(), nil
		}
		info, err := os.Stat(full)
		if err != nil || info.IsDir() || info.Size() == 0 {
			return false, firstSHA, fmt.Sprintf("missing or empty required artifact %q", rel), nil
		}

		sha, err := HashFileSHA256(full)
		if err != nil {
			return false, firstSHA, fmt.Sprintf("hash artifact %q: %v", rel, err), nil
		}
		if firstSHA == "" {
			firstSHA = sha
		}

		if isDisallowedHash(sha, spec.DisallowedHashes[rel]) || isDisallowedHash(sha, spec.DisallowedHashes["*"]) {
			return false, sha, fmt.Sprintf("stale artifact rejected: %q matches disallowed predecessor sha256 %s", rel, sha), nil
		}

		raw, err := os.ReadFile(full)
		if err != nil {
			return false, sha, fmt.Sprintf("read artifact %q: %v", rel, err), nil
		}
		if reqKeys := spec.RequiredJSONKeys[rel]; len(reqKeys) > 0 {
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				return false, sha, fmt.Sprintf("artifact %q invalid JSON: %v", rel, err), nil
			}
			for _, k := range reqKeys {
				v, exists := obj[k]
				if !exists || v == nil {
					return false, sha, fmt.Sprintf("artifact %q missing non-null JSON key %q", rel, k), nil
				}
			}
			if firstJSON == nil {
				firstJSON = raw
			}
		} else if firstJSON == nil && strings.HasSuffix(strings.ToLower(rel), ".json") {
			firstJSON = raw
		}
	}

	if len(spec.MetricThresholds) > 0 {
		metricsBytes := firstJSON
		if spec.MetricsFile != "" {
			mp, err := ResolveSafePath(wsDir, spec.MetricsFile)
			if err != nil {
				return false, firstSHA, err.Error(), nil
			}
			metricsBytes, err = os.ReadFile(mp)
			if err != nil {
				return false, firstSHA, fmt.Sprintf("read metrics_file %q: %v", spec.MetricsFile, err), nil
			}
		}
		if len(metricsBytes) == 0 {
			return false, firstSHA, "no JSON metrics payload found for threshold checks", nil
		}
		if err := CheckMetricThresholds(metricsBytes, spec.MetricThresholds); err != nil {
			return false, firstSHA, err.Error(), nil
		}
	}

	return true, firstSHA, "", nil
}

func isDisallowedHash(actual string, list []string) bool {
	for _, bad := range list {
		if strings.EqualFold(strings.TrimSpace(bad), actual) {
			return true
		}
	}
	return false
}
