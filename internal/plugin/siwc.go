package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

const siwcTimeout = time.Minute
const siwcOffMessage = "请先开启「使用 SIWC」；关闭时 CPA 会使用原始 Codex 通道，无法进行 SIWC 检测"
const siwcGenericError = "SIWC 检测失败，请稍后重试"

type siwcRecord struct {
	record
	Code       string `json:"code,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func siwcRunResponse(body []byte) managementResponse {
	var req runRequest
	if json.Unmarshal(body, &req) != nil {
		return jsonError(http.StatusBadRequest, "请求格式错误")
	}
	req.Model = strings.TrimSpace(req.Model)
	if !validModelName(req.Model, false) {
		return jsonError(http.StatusBadRequest, "请选择模型")
	}
	auths, err := selectedCredentials(req.CredentialIDs, req.All)
	if err != nil {
		return jsonError(http.StatusBadGateway, "读取凭证失败，请刷新后重试")
	}
	auths = slices.DeleteFunc(auths, func(auth credential) bool {
		return auth.Source != credentialSourceFile || auth.Provider != "codex"
	})
	return startCredentialRuns("siwc", req, auths, 1, func(id string) {
		records.SIWC.add(id, siwcRecord{record: skippedRecord(req.Model)})
	}, func(ctx context.Context, auth credential, p *progress) {
		r := evaluateSIWC(ctx, auth, req.Model)
		if ctx.Err() != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		records.SIWC.add(auth.ID, r)
		p.Done++
		_ = saveStateLocked()
	})
}

func evaluateSIWC(ctx context.Context, auth credential, model string) (r siwcRecord) {
	started := time.Now()
	r = siwcRecord{record: newRecord(model, started)}
	defer func() {
		r.DurationMS = time.Since(started).Milliseconds()
		if recovered := recover(); recovered != nil {
			// Metadata and host panic values can contain tokens; persist only a fixed message.
			r.Status, r.Error, r.Code, r.HTTPStatus = "failed", "SIWC 检测异常", "", 0
		}
	}()
	if ctx.Err() != nil {
		r.Status, r.Error = "cancelled", "SIWC 检测已停止"
		return r
	}
	// Check persisted credentials immediately before selecting the upstream route.
	metadata, err := readCodexMetadata(auth)
	if err != nil {
		r.Status, r.Error = "failed", "无法读取 SIWC 绑定状态，请刷新后重试"
		return r
	}
	bound, enabled := metadata.siwcState()
	if !bound {
		r.Status, r.Error = "skipped", "凭证尚未绑定 SIWC"
		return r
	}
	if !enabled {
		r.Status, r.Error = "skipped", siwcOffMessage
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, siwcTimeout)
	defer cancel()
	// No Codex client markers or Responses Lite context: those select the outer OAuth partition.
	body := map[string]any{
		"model": model, "store": false, "stream": true,
		"input": []map[string]string{{"role": "user", "content": "Reply with OK."}},
	}
	out, err := executeResponse(ctx, auth, model, body, http.Header{
		"Content-Type": {"application/json"}, "Accept": {"text/event-stream"},
		"User-Agent": {"cpa-siwc-check/" + pluginVersion},
	})
	r.DurationMS = time.Since(started).Milliseconds()
	r.usage = out.tokenUsage()
	if err == nil && out.completed() {
		r.Status = "available"
		return r
	}
	message := string(out.Error)
	if err != nil {
		message = err.Error()
		var envelope *EnvelopeError
		if errors.As(err, &envelope) {
			message, r.HTTPStatus = envelope.Message, envelope.HTTPStatus
		}
	}
	r.Code, r.Error = siwcErrorDetails(message)
	r.Status = "failed"
	switch r.Code {
	case "subscription_sharing_user_not_eligible":
		r.Status = "restricted"
	case "subscription_sharing_usage_limit_exceeded":
		r.Status = "limited"
	case "subscription_sharing_invalid_user":
		r.Status = "auth_error"
	}
	if r.Status == "failed" {
		switch r.HTTPStatus {
		case 401:
			r.Status, r.Error = "auth_error", "SIWC 认证异常，请重新绑定凭证"
		case 429:
			r.Status, r.Error = "limited", "SIWC 额度受限，请稍后重试"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		r.Error = "SIWC 检测超时，请稍后重试"
	}
	return r
}

// Only an exact allowlisted code in a response object (or its one-level error object) is evidence.
// Never inspect message/detail, decode nested JSON strings, truncate codes, or return upstream prose:
// those fields can contain arbitrarily encoded credentials that a token blacklist cannot safely scrub.
func siwcErrorDetails(message string) (code, detail string) {
	message = strings.TrimSpace(message)
	// These exact prefixes are added by readResponseStream, not discovered inside upstream prose.
	for _, prefix := range []string{"模型返回错误：", "模型响应流中断："} {
		if strings.HasPrefix(message, prefix) {
			message = strings.TrimSpace(strings.TrimPrefix(message, prefix))
			break
		}
	}
	type structuredError struct {
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	var data structuredError
	if !strings.HasPrefix(message, "{") || json.Unmarshal([]byte(message), &data) != nil {
		return "", siwcGenericError
	}
	if nested := strings.TrimSpace(string(data.Error)); nested != "" && nested != "null" {
		var inner structuredError
		if !strings.HasPrefix(nested, "{") || json.Unmarshal(data.Error, &inner) != nil {
			return "", siwcGenericError
		}
		// Deeper envelopes and string-encoded objects are deliberately unsupported, not recursively mined.
		if deeper := strings.TrimSpace(string(inner.Error)); deeper != "" && deeper != "null" {
			return "", siwcGenericError
		}
		data = inner
	}
	switch data.Code {
	case "subscription_sharing_user_not_eligible":
		return "subscription_sharing_user_not_eligible", "当前账号不满足 SIWC 订阅共享条件"
	case "subscription_sharing_usage_limit_exceeded":
		return "subscription_sharing_usage_limit_exceeded", "SIWC 额度已用尽，请稍后重试"
	case "subscription_sharing_invalid_user":
		return "subscription_sharing_invalid_user", "SIWC 认证异常，请重新绑定凭证"
	case "subscription_sharing_route_not_supported":
		return "subscription_sharing_route_not_supported", "当前模型或请求通道不支持 SIWC"
	case "subscription_sharing_usage_unavailable":
		return "subscription_sharing_usage_unavailable", "暂时无法获取 SIWC 用量，请稍后重试"
	default:
		return "", siwcGenericError
	}
}
