package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/system_setting"
)

// WebhookPayload webhook 通知的负载数据
type WebhookPayload struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Values    []any  `json:"values,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// generateSignature 生成 webhook 签名
func generateSignature(secret string, payload []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

func SendWebhookNotify(webhookURL string, secret string, data dto.Notify) error {
	return SendWebhookNotifyWithContext(context.Background(), webhookURL, secret, data)
}

// SendWebhookNotifyWithContext preserves the existing signed wire format while
// bounding direct and Worker requests to the caller's lifetime.
func SendWebhookNotifyWithContext(ctx context.Context, webhookURL string, secret string, data dto.Notify) error {
	payload, err := common.Marshal(WebhookPayload{
		Type: data.Type, Title: data.Title, Content: notificationContent(data), Values: data.Values, Timestamp: time.Now().Unix(),
	})
	if err != nil {
		return &notificationTransportError{cause: err}
	}
	headers := map[string]string{"Content-Type": "application/json"}
	useWorker := system_setting.EnableWorker()
	if secret != "" {
		headers["X-Webhook-Signature"] = generateSignature(secret, payload)
		if useWorker {
			headers["Authorization"] = "Bearer " + secret
		}
	}
	return sendNotificationRequest(ctx, useWorker, http.MethodPost, webhookURL, headers, payload)
}
