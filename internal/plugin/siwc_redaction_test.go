package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

// A single escaped character defeats raw token replacement. Error handling must not depend on
// recognizing any representation of a secret: upstream diagnostic text is never returned at all.
func quoteWithUnicodeEscape(t *testing.T, text string, at int) string {
	t.Helper()
	quoted, err := json.Marshal(text)
	if err != nil || len(quoted) != len(text)+2 || at < 0 || at >= len(text) {
		t.Fatal("invalid ASCII redaction fixture")
	}
	return string(quoted[:at+1]) + fmt.Sprintf(`\u%04x`, text[at]) + string(quoted[at+2:])
}

func assertNoTokenFragments(t *testing.T, data []byte, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		for start := 0; start+16 <= len(secret); start++ {
			if bytes.Contains(data, []byte(secret[start:start+16])) {
				t.Fatal("a credential token fragment reached an error, state file or UI response")
			}
		}
	}
}

func TestSIWCUntrustedDiagnosticsNeverPersist(t *testing.T) {
	secrets := []string{
		"access-sentinel-" + strings.Repeat("Ac3_", 180) + "-access-end",
		"refresh-sentinel-" + strings.Repeat("Rf4_", 180) + "-refresh-end",
		"client-sentinel-" + strings.Repeat("Cl5_", 180) + "-client-end",
		"id-sentinel-" + strings.Repeat("Id6_", 180) + "-id-end",
	}
	metadata := codexMetadata{IDToken: secrets[3], UseSIWC: json.RawMessage(`true`)}
	metadata.SIWC.AccessToken, metadata.SIWC.RefreshToken, metadata.SIWC.ClientID = secrets[0], secrets[1], secrets[2]
	file, err := json.Marshal(map[string]any{"json": metadata})
	if err != nil {
		t.Fatal(err)
	}
	for i, token := range secrets {
		for _, escapedAt := range []int{1, len(token) - 2} {
			quoted := quoteWithUnicodeEscape(t, token, escapedAt)
			type diagnostic struct{ name, wire string }
			cases := []diagnostic{
				{"message", `{"code":"probe_failed","message":` + quoted + `}`},
				{"unknown-secret-code", `{"code":` + quoted + `,"message":"unavailable"}`},
				{"detail", `{"code":"probe_failed","detail":` + quoted + `}`},
				{"nested-error", `{"error":{"error":{"code":` + quoted + `,"message":` + quoted + `}}}`},
				{"nested-error-detail", `{"error":{"code":"probe_failed","message":"","detail":` + quoted + `}}`},
				{"string-error", `{"error":` + quoted + `}`},
				{"root-string", quoted},
				{"prefixed-json", `模型返回错误：{"code":"probe_failed","message":` + quoted + `}`},
				{"plain-text", "before " + token + " after"},
				{"plain-crosses-limit", strings.Repeat("p", 480) + token + " after"},
				{"malformed-json", `{"message":` + quoted},
				{"wrong-json-type", `{"message":[` + quoted + `]}`},
				{"array-error", `{"error":[` + quoted + `]}`},
				{"untrusted-prefix", `upstream prose: {"code":"subscription_sharing_user_not_eligible","message":` + quoted + `}`},
				{"known-code-with-trailing-prose", `{"code":"subscription_sharing_user_not_eligible"} ` + token},
			}
			// A known code inside diagnostic prose must never become classification evidence, regardless
			// of how many times that prose has been JSON-encoded. Six layers also contain long escape runs.
			encoded := `{"error":{"code":"subscription_sharing_user_not_eligible","message":` + quoted + `}}`
			for layer := 1; layer <= 6; layer++ {
				wire, _ := json.Marshal(encoded)
				encoded = string(wire)
				for _, field := range []string{"message", "detail"} {
					cases = append(cases,
						diagnostic{fmt.Sprintf("encoded-%s-%d", field, layer), `{"code":"probe_failed","` + field + `":` + encoded + `}`},
						diagnostic{fmt.Sprintf("encoded-error-%s-%d", field, layer), `{"error":{"` + field + `":` + encoded + `}}`},
					)
				}
				cases = append(cases,
					diagnostic{fmt.Sprintf("encoded-root-%d", layer), encoded},
					diagnostic{fmt.Sprintf("encoded-error-object-%d", layer), `{"error":` + encoded + `}`},
					diagnostic{fmt.Sprintf("encoded-code-%d", layer), `{"code":` + encoded + `}`},
				)
			}
			for _, boundary := range []int{119, 120, 121, 499, 500, 501} {
				// No truncation may turn an unknown, secret-bearing code into a returned prefix.
				value := "subscription_sharing_user_not_eligible" + strings.Repeat("x", boundary) + token
				codeJSON := quoteWithUnicodeEscape(t, value, len(value)-2)
				cases = append(cases, diagnostic{fmt.Sprintf("code-boundary-%d", boundary), `{"code":` + codeJSON + `}`})
			}
			for _, tc := range cases {
				t.Run(fmt.Sprintf("token-%d/escape-%d/%s", i, escapedAt, tc.name), func(t *testing.T) {
					setupTest(t)
					code, detail := siwcErrorDetails(tc.wire)
					if code != "" || detail != siwcGenericError {
						t.Fatal("untrusted diagnostics were not reduced to the fixed generic result")
					}
					assertNoTokenFragments(t, []byte(code+detail), secrets)
					hostCall = func(method string, _ any) (json.RawMessage, error) {
						switch method {
						case "host.auth.list":
							return json.RawMessage(`{"files":[{"id":"a","auth_index":"index","provider":"codex"}]}`), nil
						case "host.auth.get":
							return file, nil
						case "host.model.execute_stream":
							return nil, &EnvelopeError{Code: "upstream", Message: tc.wire, HTTPStatus: http.StatusForbidden}
						default:
							t.Errorf("unexpected host call %s", method)
							return nil, fmt.Errorf("unexpected call")
						}
					}
					r := evaluateSIWC(context.Background(), credential{ID: "a", Provider: "codex", Source: credentialSourceFile, AuthIndex: "index"}, "model")
					if r.Status != "failed" || r.Code != "" || r.Error != siwcGenericError || r.HTTPStatus != http.StatusForbidden {
						t.Fatal("SIWC evaluation did not use fixed diagnostics and preserve HTTP status")
					}
					records.SIWC.add("a", r)
					if err := saveStateLocked(); err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(statePath)
					if err != nil {
						t.Fatal(err)
					}
					assertNoTokenFragments(t, data, secrets)
					response := stateResponse()
					if response.StatusCode != http.StatusOK {
						t.Fatal("state response failed")
					}
					assertNoTokenFragments(t, response.Body, secrets)
				})
			}
		}
	}
}

func TestSIWCOnlyAuthoritativeKnownCodesClassify(t *testing.T) {
	for known, prompt := range map[string]string{
		"subscription_sharing_user_not_eligible":    "当前账号不满足 SIWC 订阅共享条件",
		"subscription_sharing_usage_limit_exceeded": "SIWC 额度已用尽，请稍后重试",
		"subscription_sharing_invalid_user":         "SIWC 认证异常，请重新绑定凭证",
		"subscription_sharing_route_not_supported":  "当前模型或请求通道不支持 SIWC",
		"subscription_sharing_usage_unavailable":    "暂时无法获取 SIWC 用量，请稍后重试",
	} {
		t.Run(known, func(t *testing.T) {
			// Known codes return a literal safe message, even if their diagnostic prose contains tokens
			// or conflicting codes. Those prose fields are not inspected and cannot override the code.
			for _, wire := range []string{
				`{"code":"` + known + `","message":"` + secretSentinel + `"}`,
				`{"error":{"code":"` + known + `","detail":"` + secretSentinel + `"}}`,
				`{"code":"` + known + `","message":"{\"code\":\"subscription_sharing_invalid_user\"}"}`,
				`{"error":{"code":"` + known + `","detail":"{\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\"}}"}}`,
				`模型返回错误：{"type":"error","code":"` + known + `"}`,
				`模型响应流中断：{"error":{"code":"` + known + `"}}`,
			} {
				code, detail := siwcErrorDetails(wire)
				if code != known || detail != prompt {
					t.Fatal("authoritative code or fixed prompt changed")
				}
				assertNoSecrets(t, []byte(code+detail))
			}
			for _, wire := range []string{
				`{"message":"{\"code\":\"` + known + `\"}"}`,
				`{"detail":"{\"code\":\"` + known + `\"}"}`,
				`{"code":"probe_failed","message":{"code":"` + known + `"}}`,
				`{"error":{"detail":{"code":"` + known + `"}}}`,
				`{"error":{"error":{"code":"` + known + `"}}}`,
				`{"error":"{\"code\":\"` + known + `\"}"}`,
				`"{\"code\":\"` + known + `\"}"`,
				`{"code":"` + known + `x"}`,
				`{"code":" ` + known + ` "}`,
				`{"code":["` + known + `"]}`,
			} {
				code, detail := siwcErrorDetails(wire)
				if code != "" || detail != siwcGenericError {
					t.Fatal("diagnostic prose or an inexact code was used for SIWC classification")
				}
			}
		})
	}
}

func TestSIWCHTTPStatusFallbackUsesFixedPrompts(t *testing.T) {
	for _, tc := range []struct {
		httpStatus int
		status     string
		detail     string
	}{
		{http.StatusUnauthorized, "auth_error", "SIWC 认证异常，请重新绑定凭证"},
		{http.StatusTooManyRequests, "limited", "SIWC 额度受限，请稍后重试"},
		{http.StatusForbidden, "failed", siwcGenericError},
		{http.StatusServiceUnavailable, "failed", siwcGenericError},
	} {
		t.Run(fmt.Sprint(tc.httpStatus), func(t *testing.T) {
			setupTest(t)
			hostCall = func(method string, _ any) (json.RawMessage, error) {
				if method == "host.auth.get" {
					return siwcMetadata(true), nil
				}
				return nil, &EnvelopeError{Code: "upstream", HTTPStatus: tc.httpStatus, Message: `{"message":"{\"code\":\"subscription_sharing_user_not_eligible\",\"detail\":\"` + secretSentinel + `\"}"}`}
			}
			r := evaluateSIWC(context.Background(), credential{ID: "a", Provider: "codex", Source: credentialSourceFile, AuthIndex: "index"}, "model")
			if r.HTTPStatus != tc.httpStatus || r.Status != tc.status || r.Code != "" || r.Error != tc.detail {
				t.Fatal("HTTP status fallback did not preserve its classification and fixed prompt")
			}
			assertNoSecrets(t, []byte(r.Code+r.Error))
		})
	}
}
