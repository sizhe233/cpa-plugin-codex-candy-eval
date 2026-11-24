package fingerprint

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// Fixed public enrollment responses from Ikaleio/lm-detector (MIT; see data/LICENSE), at the same
// pinned revision as the detector. This is offline classification regression, not an accuracy benchmark.
// No host executor, network or real model calls are part of this test.
func TestGPT62FixedReferenceSamples(t *testing.T) {
	data, err := os.ReadFile("testdata/gpt62.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Commit string `json:"source_commit"`
		Cases  []struct {
			Model   string `json:"model"`
			Outputs []struct {
				Path          string `json:"path"`
				Text          string `json:"text"`
				SHA256        string `json:"text_sha256"`
				ExpectedCount int    `json:"expected_count"`
			} `json:"outputs"`
		} `json:"cases"`
	}
	if json.Unmarshal(data, &fixture) != nil || fixture.Commit != "33fb43f26670be0ed7817ab13e5c4c4731ccb76a" || len(fixture.Cases) != 2 {
		t.Fatal("reference fixture provenance changed")
	}
	seen := map[string]bool{}
	for _, tc := range fixture.Cases {
		t.Run(tc.Model, func(t *testing.T) {
			if (tc.Model != "gpt-6.2-sol" && tc.Model != "gpt-6.2-astra") || seen[tc.Model] || len(tc.Outputs) != ChallengeCount {
				t.Fatal("missing or duplicate GPT-6.2 reference group")
			}
			seen[tc.Model] = true
			replies := make([]Reply, len(tc.Outputs))
			for i, output := range tc.Outputs {
				path := fmt.Sprintf("data/collections/%s-openrouter-openai-low-20261118/attempts/query-%02d/0001.json", tc.Model, i+1)
				if output.Path != path || fmt.Sprintf("%x", sha256.Sum256([]byte(output.Text))) != output.SHA256 || output.ExpectedCount != suite[i].ExpectedCount {
					t.Fatal("fixed response bytes or challenge counts changed")
				}
				replies[i] = Prepare(output.Text, output.ExpectedCount)
				if !replies[i].Valid() {
					t.Fatal("fixed reference response no longer accepted")
				}
			}
			got := Analyze(replies)
			if got == nil || got.Prediction != tc.Model || got.Used != ChallengeCount || got.Decision != "" || got.Probability == nil || math.IsNaN(*got.Probability) || *got.Probability <= 0 || *got.Probability > 1 {
				t.Fatalf("GPT-6.2 fixed reference attribution regressed: %+v", got)
			}
			if got.Results[0].Model != tc.Model || !strings.HasPrefix(got.PredictionName, tc.Model) {
				t.Fatal("reference result and display attribution disagree")
			}
			partial := Analyze(replies[:2])
			if partial == nil || partial.Decision != "partial" || partial.Used != 2 || partial.Probability != nil {
				t.Fatal("partial GPT-6.2 samples must not claim calibrated confidence")
			}
		})
	}
}
