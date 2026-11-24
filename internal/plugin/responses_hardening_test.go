package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func completedStreamChunk() json.RawMessage {
	data, _ := json.Marshal(map[string]any{"done": true, "payload": []byte(`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n")})
	return data
}

func TestExecuteResponseClosesOnceOnEveryExit(t *testing.T) {
	for _, mode := range []string{"success", "read-error", "read-panic", "invalid-chunk", "close-error", "close-panic", "invalid-open", "missing-id", "open-panic"} {
		t.Run(mode, func(t *testing.T) {
			setupTest(t)
			var closes atomic.Int32
			hostCall = func(method string, _ any) (json.RawMessage, error) {
				switch method {
				case "host.model.execute_stream":
					switch mode {
					case "open-panic":
						panic(secretSentinel)
					case "invalid-open":
						return json.RawMessage(secretSentinel), nil
					case "missing-id":
						return json.RawMessage(`{"unexpected":"` + secretSentinel + `"}`), nil
					}
					return json.RawMessage(`{"stream_id":"s"}`), nil
				case "host.model.stream_read":
					switch mode {
					case "read-error":
						return nil, errors.New("stream unavailable")
					case "read-panic":
						panic(secretSentinel)
					case "invalid-chunk":
						return json.RawMessage(`{"payload":`), nil
					}
					return completedStreamChunk(), nil
				case "host.model.stream_close":
					closes.Add(1)
					switch mode {
					case "close-error":
						return nil, errors.New(secretSentinel)
					case "close-panic":
						panic(secretSentinel)
					}
					return json.RawMessage(`{}`), nil
				default:
					t.Errorf("unexpected host call %s", method)
					return nil, errors.New("unexpected call")
				}
			}
			_, err := executeResponse(context.Background(), credential{ID: "a", Provider: "codex"}, "m", nil, nil)
			wantCloses := int32(1)
			if mode == "invalid-open" || mode == "missing-id" || mode == "open-panic" {
				wantCloses = 0
			}
			if closes.Load() != wantCloses || (err == nil) != (mode == "success") {
				t.Fatalf("close count=%d expected=%d; error present=%t", closes.Load(), wantCloses, err != nil)
			}
			if err != nil {
				assertNoSecrets(t, []byte(err.Error()))
			}
		})
	}
}

func TestExecuteResponseCancellationAndDeadlineCloseSafely(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		for _, closeFailure := range []string{"none", "error", "panic"} {
			t.Run(fmt.Sprintf("deadline=%t/close=%s", timeout, closeFailure), func(t *testing.T) {
				setupTest(t)
				entered, closed := make(chan struct{}), make(chan struct{})
				var once sync.Once
				var closes atomic.Int32
				ctx, cancel := context.WithCancel(context.Background())
				if timeout {
					ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
				}
				defer cancel()
				hostCall = func(method string, _ any) (json.RawMessage, error) {
					switch method {
					case "host.model.execute_stream":
						return json.RawMessage(`{"stream_id":"s"}`), nil
					case "host.model.stream_read":
						close(entered)
						<-closed
						return nil, errors.New("closed")
					case "host.model.stream_close":
						closes.Add(1)
						once.Do(func() { close(closed) })
						switch closeFailure {
						case "error":
							return nil, errors.New(secretSentinel)
						case "panic":
							panic(secretSentinel)
						}
						return json.RawMessage(`{}`), nil
					default:
						return nil, errors.New("unexpected call")
					}
				}
				finished := make(chan error, 1)
				go func() {
					_, err := executeResponse(ctx, credential{ID: "a", Provider: "codex"}, "m", nil, nil)
					finished <- err
				}()
				<-entered
				if !timeout {
					cancel()
				}
				select {
				case err := <-finished:
					want := context.Canceled
					if timeout {
						want = context.DeadlineExceeded
					}
					if !errors.Is(err, want) || closes.Load() != 1 {
						t.Fatal("cancellation lost its cause or failed to close exactly once")
					}
					assertNoSecrets(t, []byte(err.Error()))
				case <-time.After(3 * time.Second):
					t.Fatal("cancelled stream remained blocked")
				}
			})
		}
	}
}

func TestCancelledEvaluationNeverStartsHostCalls(t *testing.T) {
	setupTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	hostCall = func(string, any) (json.RawMessage, error) { calls.Add(1); return nil, nil }
	auth := credential{ID: "a", Provider: "codex", Source: credentialSourceFile, AuthIndex: "a"}
	if _, err := executeResponse(ctx, auth, "m", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("already cancelled request was accepted")
	}
	if result := evaluateSIWC(ctx, auth, "m"); result.Status != "cancelled" {
		t.Fatal("already cancelled SIWC was accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("cancelled evaluation reached the host")
	}
}
