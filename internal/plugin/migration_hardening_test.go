package plugin

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Synthetic schema-2 records use every historical data shape, including numbers above JS's safe integer
// range, fingerprint probabilities/diagnostics and both pelican variants. No model or production data.
const schemaTwoHistory = `{
  "schema_version": 2,
  "candies": {
    "a": [
      {"id":"c1","time":"2026-10-29T00:00:00.123456789Z","model":"gpt-6.1-sol","status":"correct","duration_ms":19,"effort":"low","answer":"答案：21 <svg>&","attempts":2,"input_tokens":9007199254740993,"output_tokens":31,"reasoning_tokens":11},
      {"id":"c2","time":"2026-10-29T00:01:00Z","model":"old-model","status":"wrong","duration_ms":3,"effort":"high","answer":"20","attempts":1,"input_tokens":13,"output_tokens":2,"reasoning_tokens":0},
      {"id":"c3","time":"2026-10-29T00:02:00Z","model":"old-model","status":"failed","duration_ms":8,"effort":"low","error":"synthetic network failure","attempts":3,"input_tokens":4,"output_tokens":0,"reasoning_tokens":0}
    ],
    "orphan": [{"id":"c4","time":"2026-10-29T00:03:00Z","model":"removed-model","status":"skipped","duration_ms":0,"effort":"none","error":"unsupported","input_tokens":0,"output_tokens":0,"reasoning_tokens":0}]
  },
  "fingerprints": {
    "a": [{"id":"f1","time":"2026-10-29T00:04:00Z","model":"gpt-6.1-sol","status":"completed","duration_ms":300,"input_tokens":30000,"output_tokens":300,"reasoning_tokens":7,"detector_revision":"old-bank-revision","concurrency":3,
      "samples":[{"instructions":"original system","prompt":"original challenge","expected_count":3,"attempts":2,"text":"1, 355, 17","parsed_numbers":3,"accepted":true},{"prompt":"failed challenge","expected_count":300,"attempts":3,"error":"temporary failure","parsed_numbers":0,"accepted":false}],
      "attribution":{"prediction":"gpt-6-astra","prediction_name":"gpt-6-astra/6.1-sol","probability":0.987654321,"used_outputs":3,"family_prediction_name":"GPT","sequence":{"autocorrelation":-0.52,"small_step_share":0.014,"model_like":true},"results":[{"model":"gpt-6-astra","display_name":"gpt-6-astra/6.1-sol","family":"gpt","family_name":"GPT","probability":0.987654321,"score":1.001}],"diagnostics":[{"index":0,"parsed_numbers":300,"minimum_numbers":165,"accepted":true}]}}],
    "orphan": [{"id":"f2","time":"2026-10-29T00:05:00Z","model":"old-model","status":"partial","duration_ms":400,"input_tokens":15,"output_tokens":5,"reasoning_tokens":0,"detector_revision":"older-revision","samples":[],"attribution":{"decision":"partial","prediction":"gpt-6-astra","probability":null,"used_outputs":1,"family_prediction_name":"GPT","results":[{"model":"gpt-6-astra","display_name":"old display","family":"gpt","family_name":"GPT","probability":null,"score":0.98}],"diagnostics":[]}}]
  },
  "pelicans": {
    "a": [{"id":"p1","time":"2026-10-29T00:06:00Z","model":"old-model","status":"completed","duration_ms":111,"input_tokens":19,"output_tokens":22,"reasoning_tokens":3,"effort":"high","variant":"original","attempts":1,"html":"<!doctype html><svg><text>中文 &amp; 21</text></svg>"},
      {"id":"p2","time":"2026-10-29T00:07:00Z","model":"old-model","status":"completed","duration_ms":222,"input_tokens":20,"output_tokens":23,"reasoning_tokens":4,"effort":"max","variant":"all-in-one","attempts":2,"html":"<html><body><svg></svg></body></html>"}]
  }
}`

func decodeSemanticJSON(t *testing.T, data []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSchemaTwoMigrationPreservesEveryHistory(t *testing.T) {
	setupTest(t)
	original := []byte(schemaTwoHistory)
	if err := os.WriteFile(statePath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	loadState()
	if storageError != "" || stateLoadError != nil || records.SchemaVersion != 3 || len(records.SIWC) != 0 {
		t.Fatal("schema-2 migration failed or produced SIWC history")
	}
	saved, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	before, after := decodeSemanticJSON(t, original), decodeSemanticJSON(t, saved)
	// The typed representation adds harmless zero fields for omitted values; normalize the expected
	// histories through the same public record shapes and compare every field, ordering and account key.
	var legacy struct {
		SchemaVersion int                        `json:"schema_version"`
		Candies       history[candyRecord]       `json:"candies"`
		Fingerprints  history[fingerprintRecord] `json:"fingerprints"`
		Pelicans      history[pelicanRecord]     `json:"pelicans"`
	}
	if err := json.Unmarshal(original, &legacy); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy.Candies, records.Candies) || !reflect.DeepEqual(legacy.Fingerprints, records.Fingerprints) || !reflect.DeepEqual(legacy.Pelicans, records.Pelicans) {
		t.Fatal("history fields, credential keys or order changed")
	}
	for _, name := range []string{"candies", "fingerprints", "pelicans"} {
		assertHistoricalFields(t, before[name], after[name])
	}
	if siwc, ok := after["siwc"].(map[string]any); !ok || len(siwc) != 0 {
		t.Fatal("schema 3 must add an empty SIWC object")
	}
	backupPath := strings.TrimSuffix(statePath, ".json") + ".v2.json"
	backup, err := os.ReadFile(backupPath)
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatal("rollback copy is not byte-identical schema 2")
	}
	// A legacy reader must consume the rollback bytes without any schema-3 SIWC field.
	decoder := json.NewDecoder(bytes.NewReader(backup))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil || legacy.SchemaVersion != 2 {
		t.Fatal("rollback copy is incompatible with the schema-2 reader")
	}
	first := records
	records, loaded = emptyState(), false
	loadState()
	if !reflect.DeepEqual(first, records) || storageError != "" {
		t.Fatal("schema-3 reload changed migrated history")
	}
	info, err := os.Stat(backupPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("rollback backup is not private")
	}
}

// Check each raw historical field too, so a new struct that omits a legacy field cannot make both sides
// of a typed comparison silently discard that field. New zero-value fields do not count as lost history.
func assertHistoricalFields(t *testing.T, before, after any) {
	t.Helper()
	switch before := before.(type) {
	case map[string]any:
		after, ok := after.(map[string]any)
		if !ok {
			t.Fatal("historical object changed type")
		}
		for key, value := range before {
			assertHistoricalFields(t, value, after[key])
		}
	case []any:
		after, ok := after.([]any)
		if !ok || len(before) != len(after) {
			t.Fatal("historical list lost entries")
		}
		for i, value := range before {
			assertHistoricalFields(t, value, after[i])
		}
	default:
		if !reflect.DeepEqual(before, after) {
			t.Fatal("historical scalar changed")
		}
	}
}

func TestStateMigrationRejectsLossyOrInvalidHistory(t *testing.T) {
	overLimit := `{"schema_version":2,"candies":{"a":[` + strings.Repeat(`{"id":"x","status":"correct"},`, historyLimit) + `{"id":"last","status":"correct"}]}}`
	for name, input := range map[string]string{
		"null-version":       `{"schema_version":null,"candies":{"a":[]}}`,
		"negative-version":   `{"schema_version":-1}`,
		"fractional-version": `{"schema_version":2.5}`,
		"future-version":     `{"schema_version":99}`,
		"null-record":        `{"schema_version":2,"candies":{"a":[null]}}`,
		"wrong-history":      `{"schema_version":2,"fingerprints":[]}`,
		"unknown-record":     `{"schema_version":2,"pelicans":{"a":[{"future_history":"must not disappear"}]}}`,
		"existing-siwc":      `{"schema_version":2,"siwc":{"a":[{"id":"already-present"}]}}`,
		"over-limit":         overLimit,
		"corrupt":            `{"schema_version":2,"candies":`,
	} {
		t.Run(name, func(t *testing.T) {
			setupTest(t)
			if err := os.WriteFile(statePath, []byte(input), 0o600); err != nil {
				t.Fatal(err)
			}
			loadState()
			if stateLoadError == nil || storageError == "" || saveStateLocked() == nil {
				t.Fatal("invalid history was silently accepted or save remained enabled")
			}
			got, err := os.ReadFile(statePath)
			if err != nil || string(got) != input {
				t.Fatal("invalid state was overwritten")
			}
			if _, err := os.Stat(strings.TrimSuffix(statePath, ".json") + ".v2.json"); !os.IsNotExist(err) {
				t.Fatal("invalid migration unexpectedly created a rollback backup")
			}
		})
	}
}

func TestMigrationBackupRefusesOverwriteAndUnsafeTargets(t *testing.T) {
	for _, mode := range []string{"different", "identical", "directory", "symlink", "public"} {
		t.Run(mode, func(t *testing.T) {
			setupTest(t)
			original := []byte(schemaTwoHistory)
			if err := os.WriteFile(statePath, original, 0o600); err != nil {
				t.Fatal(err)
			}
			backup := strings.TrimSuffix(statePath, ".json") + ".v2.json"
			var previous []byte
			switch mode {
			case "identical":
				previous = original
			case "different", "public":
				previous = []byte(`{"schema_version":2,"candies":{"older":[]}}`)
			case "directory":
				if err := os.Mkdir(backup, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "rollback.json")
				if err := os.WriteFile(target, original, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, backup); err != nil {
					t.Fatal(err)
				}
			}
			if previous != nil {
				if err := os.WriteFile(backup, previous, 0o600); err != nil {
					t.Fatal(err)
				}
				if mode == "public" {
					if err := os.Chmod(backup, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			loadState()
			if mode == "identical" {
				if stateLoadError != nil || records.SchemaVersion != 3 {
					t.Fatal("identical rollback backup should be reusable")
				}
			} else {
				if stateLoadError == nil || saveStateLocked() == nil {
					t.Fatal("unsafe backup target did not block saving")
				}
				data, _ := os.ReadFile(statePath)
				if !bytes.Equal(data, original) {
					t.Fatal("state changed without a verified backup")
				}
			}
			if previous != nil {
				data, _ := os.ReadFile(backup)
				if !bytes.Equal(data, previous) {
					t.Fatal("migration overwrote an existing rollback backup")
				}
			}
		})
	}
}
