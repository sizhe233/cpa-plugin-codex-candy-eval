package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-codex-candy-eval/internal/fingerprint"
)

func TestForkAutomationAndReleaseGates(t *testing.T) {
	badge, err := os.ReadFile("../../.github/workflows/download-badge.yml")
	if err != nil || !bytes.Contains(badge, []byte("    if: ${{ github.repository == 'haowang02/cpa-plugin-codex-candy-eval' }}")) {
		t.Fatal("download badge automation must be restricted to the upstream repository")
	}
	release, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, gate := range []string{"Check tag matches plugin version", "${GITHUB_REF_NAME#v}", "go vet ./...", "go test -race ./...", "node scripts/test-ui.cjs", "quay.io/pypa/manylinux2014_x86_64", "quay.io/pypa/manylinux2014_aarch64", "sha256sum * > checksums.txt"} {
		if !bytes.Contains(release, []byte(gate)) {
			t.Fatalf("missing release gate %q", gate)
		}
	}
	module, err := os.ReadFile("../../go.mod")
	if err != nil || !bytes.Contains(module, []byte("\ngo 1.24\n")) {
		t.Fatal("minimum Go version unexpectedly changed")
	}
}

const secretSentinel = "synthetic-private-token-never-persist"

func assertNoSecrets(t *testing.T, data []byte, secrets ...string) {
	t.Helper()
	for _, secret := range append([]string{secretSentinel}, secrets...) {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("sensitive sentinel leaked")
		}
	}
}

func waitForTests(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { tasks.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background tasks did not finish")
	}
}

func TestSensitiveRoutesRemainManagementOnly(t *testing.T) {
	setupTest(t)
	var calls atomic.Int32
	hostCall = func(string, any) (json.RawMessage, error) { calls.Add(1); return nil, nil }
	var registration struct {
		Result struct {
			Routes    []map[string]any `json:"routes"`
			Resources []map[string]any `json:"resources"`
		} `json:"result"`
	}
	if err := json.Unmarshal(HandleMethod("management.register", nil), &registration); err != nil {
		t.Fatal(err)
	}
	if len(registration.Result.Routes) != len(routes) || len(registration.Result.Resources) != 1 || registration.Result.Resources[0]["Path"] != uiPath {
		t.Fatal("registration exposed unexpected resources")
	}
	for _, r := range registration.Result.Routes {
		path, _ := r["Path"].(string)
		// CPA v8.1.2 authenticates this prefix; a Menu would reclassify a GET as a public resource.
		if !strings.HasPrefix(path, managementBase+"/") || r["Menu"] != nil {
			t.Fatal("sensitive route escaped management authentication")
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			response := handleManagement(managementRequest{Method: method, Path: strings.Replace(path, "/v0/management/", "/v0/resource/", 1)})
			if response.StatusCode != http.StatusNotFound {
				t.Fatal("public resource alias dispatched a sensitive handler")
			}
		}
	}
	if response := handleManagement(managementRequest{Method: http.MethodGet, Path: uiPath}); response.StatusCode != 200 {
		t.Fatal("static UI unavailable")
	}
	if calls.Load() != 0 {
		t.Fatal("registration or public resource performed a host/model call")
	}
}

func TestManagementPanicDoesNotExposeCredentials(t *testing.T) {
	setupTest(t)
	hostCall = func(string, any) (json.RawMessage, error) { panic(secretSentinel) }
	request, _ := json.Marshal(managementRequest{Method: http.MethodGet, Path: managementBase + "/state"})
	response := HandleMethod("management.handle", request)
	assertNoSecrets(t, response)
	var result Envelope
	if json.Unmarshal(response, &result) != nil || result.OK || result.Error == nil || result.Error.HTTPStatus != 500 {
		t.Fatal("management panic was not reported safely")
	}
}

func TestStartRunsPanicReleasesAccounting(t *testing.T) {
	for _, test := range []string{"candy", "fingerprint", "pelican", "siwc"} {
		t.Run(test, func(t *testing.T) {
			setupTest(t)
			var taskCtx context.Context
			var p *progress
			auth := credential{ID: "a", Provider: "codex"}
			r := startCredentialRuns(test, runRequest{Model: "model"}, []credential{auth}, 1, func(string) {}, func(ctx context.Context, _ credential, progress *progress) {
				taskCtx, p = ctx, progress
				panic(secretSentinel)
			})
			if r.StatusCode != 200 {
				t.Fatal("run did not start")
			}
			waitForTests(t)
			if len(running) != 0 || taskCtx.Err() != context.Canceled || p.Done != 1 {
				t.Fatal("panic leaked running/count/context state")
			}
			data, err := os.ReadFile(statePath)
			if err != nil || !bytes.Contains(data, []byte(`"status":"failed"`)) {
				t.Fatal("outer run panic did not persist a failed record")
			}
			assertNoSecrets(t, data)
			var ran atomic.Bool
			r = startCredentialRuns(test, runRequest{Model: "model"}, []credential{auth}, 1, func(string) {}, func(context.Context, credential, *progress) { ran.Store(true) })
			waitForTests(t)
			if r.StatusCode != 200 || !ran.Load() || len(running) != 0 {
				t.Fatal("later task could not reuse the credential")
			}
		})
	}
}

func TestSIWCPanicProducesFailedRecordAndAllowsNextRun(t *testing.T) {
	setupTest(t)
	var fail atomic.Bool
	fail.Store(true)
	hostCall = streamHost(func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.RawMessage(`{"files":[{"id":"a","auth_index":"index","provider":"codex"}]}`), nil
		case "host.auth.get":
			if fail.Load() {
				panic(secretSentinel)
			}
			return siwcMetadata(true), nil
		case "host.model.execute_stream":
			return mockModelResponse("OK"), nil
		default:
			t.Errorf("unexpected host call %s", method)
			return nil, errors.New("unexpected call")
		}
	})
	for _, want := range []string{"failed", "available"} {
		if r := siwcRunResponse([]byte(`{"all":true,"model":"model"}`)); r.StatusCode != 200 {
			t.Fatal("SIWC did not start")
		}
		waitForTests(t)
		history := records.SIWC["a"]
		if len(history) == 0 || history[len(history)-1].Status != want || len(running) != 0 {
			t.Fatal("SIWC panic did not release the credential and preserve a result")
		}
		data, _ := os.ReadFile(statePath)
		assertNoSecrets(t, data)
		fail.Store(false)
	}
}

func TestSIWCMetadataStaysInMemory(t *testing.T) {
	for _, mode := range []string{"ok", "metadata-error", "metadata-panic", "invalid-json", "reflected-error"} {
		t.Run(mode, func(t *testing.T) {
			setupTest(t)
			secrets := []string{secretSentinel + "-access", secretSentinel + "-refresh", secretSentinel + "-client", secretSentinel + "-id"}
			metadata, _ := json.Marshal(map[string]any{"json": map[string]any{
				"use_siwc": true, "plan_type": "pro", "id_token": secrets[3],
				"siwc": map[string]string{"access_token": secrets[0], "refresh_token": secrets[1], "client_id": secrets[2]},
			}})
			var modelCalls atomic.Int32
			hostCall = streamHost(func(method string, payload any) (json.RawMessage, error) {
				encoded, _ := json.Marshal(payload)
				assertNoSecrets(t, encoded)
				switch method {
				case "host.auth.list":
					return json.RawMessage(`{"files":[{"id":"a","auth_index":"index","provider":"codex","priority":-7}]}`), nil
				case "host.auth.get":
					switch mode {
					case "metadata-error":
						return nil, errors.New(string(metadata))
					case "metadata-panic":
						panic(string(metadata))
					case "invalid-json":
						return append(metadata, '!'), nil
					}
					return metadata, nil
				case "host.model.execute_stream":
					modelCalls.Add(1)
					if mode == "reflected-error" {
						message, _ := json.Marshal(map[string]any{"error": map[string]string{"code": secrets[0], "message": strings.Join(secrets, ",")}})
						return nil, &EnvelopeError{Code: "upstream", HTTPStatus: 403, Message: string(message)}
					}
					return mockModelResponse("OK"), nil
				default:
					t.Errorf("credential mutation or unexpected host call %s", method)
					return nil, errors.New("unexpected call")
				}
			})
			request, _ := json.Marshal(managementRequest{Method: http.MethodGet, Path: managementBase + "/state"})
			view := HandleMethod("management.handle", request)
			assertNoSecrets(t, view, "access_token", "refresh_token", "id_token", "client_id")
			if modelCalls.Load() != 0 {
				t.Fatal("viewing state invoked a model")
			}
			auth := credential{ID: "a", Provider: "codex", Source: credentialSourceFile, AuthIndex: "index", Priority: -7}
			r := evaluateSIWC(context.Background(), auth, "model")
			records.SIWC.add(auth.ID, r)
			if err := saveStateLocked(); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(statePath)
			assertNoSecrets(t, data)
			assertNoSecrets(t, HandleMethod("management.handle", request))
			if mode != "ok" && r.Status != "failed" {
				t.Fatal("invalid metadata or reflected error did not fail closed")
			}
			if auth.Priority != -7 || auth.UseSIWC {
				t.Fatal("evaluation mutated the credential")
			}
		})
	}
}

// An external failure before executeResponse's own recovery still must not leak a global worker slot.
type panicErrContext struct{ context.Context }

func (panicErrContext) Err() error { panic(secretSentinel) }

func TestFingerprintWorkersRecoverAndReleaseSlots(t *testing.T) {
	setupTest(t)
	p := &progress{Total: fingerprint.ChallengeCount}
	ctx := panicErrContext{context.Background()}
	// collectFingerprintSample recovers panics outside the host call (here, the context precheck).
	for range 2 * fingerprintGlobalConcurrency {
		r := collectFingerprintSample(ctx, credential{ID: "a", Provider: "codex"}, "model", fingerprint.Challenges()[0])
		if !r.finished || r.sample.Attempts != 1 || r.sample.Error == "" {
			t.Fatal("sample panic did not produce a completed failure")
		}
		assertNoSecrets(t, []byte(r.sample.Error))
	}
	hostCall = streamHost(func(method string, _ any) (json.RawMessage, error) {
		if method == "host.model.execute_stream" {
			panic(secretSentinel)
		}
		return nil, fmt.Errorf("unexpected call %s", method)
	})
	runFingerprint(context.Background(), credential{ID: "a", Provider: "codex"}, fingerprintRunRequest{runRequest: runRequest{Model: "model"}, Concurrency: fingerprint.ChallengeCount}, p)
	if len(fingerprintSlots) != 0 || p.Done != p.Total || records.Fingerprints["a"][0].Status != "failed" {
		t.Fatal("worker panic leaked slots or completion counts")
	}
	data, _ := os.ReadFile(statePath)
	assertNoSecrets(t, data)
	fingerprintTestHost(t, func() (string, int) { return strings.Repeat("10,340,", 160), 200 })
	if r := fingerprintRunResponse([]byte(`{"all":true,"model":"model"}`)); r.StatusCode != 200 {
		t.Fatal("follow-up fingerprint run did not start")
	}
	waitForTests(t)
	if len(fingerprintSlots) != 0 || len(running) != 0 || records.Fingerprints["a"][1].Status != "completed" {
		t.Fatal("follow-up fingerprint run did not complete")
	}
}

func TestSIWCCancellationReleasesCredential(t *testing.T) {
	setupTest(t)
	entered, closed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var closes atomic.Int32
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.RawMessage(`{"files":[{"id":"a","auth_index":"index","provider":"codex"}]}`), nil
		case "host.auth.get":
			return siwcMetadata(true), nil
		case "host.model.execute_stream":
			return json.RawMessage(`{"stream_id":"s"}`), nil
		case "host.model.stream_read":
			close(entered)
			<-closed
			return nil, context.Canceled
		case "host.model.stream_close":
			closes.Add(1)
			once.Do(func() { close(closed) })
			panic(secretSentinel)
		default:
			return nil, errors.New("unexpected call")
		}
	}
	if response := siwcRunResponse([]byte(`{"all":true,"model":"model"}`)); response.StatusCode != 200 {
		t.Fatal("SIWC did not start")
	}
	<-entered
	if response := cancelResponse([]byte(`{"credential_ids":["a"]}`)); response.StatusCode != 200 {
		t.Fatal("cancel failed")
	}
	waitForTests(t)
	if closes.Load() != 1 || len(running) != 0 || len(records.SIWC["a"]) != 0 {
		t.Fatal("cancelled SIWC leaked work or persisted a misleading result")
	}
}
