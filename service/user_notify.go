package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/system_setting"
)

// NotifyChannelWatchers sends a channel notice (a channel disabled or enabled
// automatically, or a test of all channels finished) to every administrator
// who receives channel notices. Until they choose, the root user receives them
// and other administrators do not; render uses each recipient's saved language.
func NotifyChannelWatchers(notifyType string, render func(lang string) (subject string, content string)) {
	notifyAdmins(notifyType, func(role int, setting dto.UserSetting) bool {
		if setting.ChannelNotifyEnabled != nil {
			return *setting.ChannelNotifyEnabled
		}
		return role == common.RoleRootUser
	}, render)
}

// NotifyUpstreamModelUpdateWatchers sends the upstream model update notice to
// every administrator who enabled it, in their saved language.
func NotifyUpstreamModelUpdateWatchers(render func(lang string) (subject string, content string)) {
	sentCount := notifyAdmins(dto.NotifyTypeChannelUpdate, func(_ int, setting dto.UserSetting) bool {
		return setting.UpstreamModelUpdateNotifyEnabled
	}, render)
	common.SysLog(common.LogText("upstream model update notifications sent: %d", sentCount))
}

func notifyAdmins(notifyType string, receives func(role int, setting dto.UserSetting) bool, render func(lang string) (subject string, content string)) int {
	var users []model.User
	if err := model.DB.
		Select("id", "email", "role", "status", "setting").
		Where("status = ? AND role >= ?", common.UserStatusEnabled, common.RoleAdminUser).
		Find(&users).Error; err != nil {
		common.SysLog(common.LogText("failed to query administrators to notify: %s", err.Error()))
		return 0
	}

	sentCount := 0
	for _, user := range users {
		userSetting := user.GetSetting()
		if !receives(user.Role, userSetting) {
			continue
		}
		subject, content := render(userSetting.Language)
		if err := NotifyUser(user.Id, user.Email, userSetting, dto.NewNotify(notifyType, subject, content, nil)); err != nil {
			common.SysLog(common.LogText("failed to send notice %s to user %d: %s", notifyType, user.Id, err.Error()))
			continue
		}
		sentCount++
	}
	return sentCount
}

type NotificationDeliveryStatus string

const (
	NotificationAccepted     NotificationDeliveryStatus = "accepted"
	NotificationUnconfigured NotificationDeliveryStatus = "unconfigured"
	NotificationRetry        NotificationDeliveryStatus = "retry"
	NotificationRejected     NotificationDeliveryStatus = "rejected"
)

// NotificationDeliveryResult contains only controlled diagnostics. Acceptance
// means the selected transport acknowledged the message, not that a person read it.
type NotificationDeliveryResult struct {
	Status NotificationDeliveryStatus
	Code   string
}

var errNotificationDestination = errors.New("invalid notification destination")

type notificationHTTPStatusError struct{ status int }

func (err *notificationHTTPStatusError) Error() string {
	return fmt.Sprintf("notification transport returned HTTP %d", err.status)
}

type notificationTransportError struct{ cause error }

func (err *notificationTransportError) Error() string { return "notification transport failed" }
func (err *notificationTransportError) Unwrap() error { return err.cause }

// DeliverUserNotification performs one attempt without the legacy in-memory
// limiter. Durable callers claim their attempt budget before entering this API.
func DeliverUserNotification(ctx context.Context, userEmail string, setting dto.UserSetting, data dto.Notify) NotificationDeliveryResult {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var err error
	useWorker := system_setting.EnableWorker()
	if err = ctx.Err(); err == nil {
		switch setting.NotifyType {
		case "", dto.NotifyTypeEmail:
			address := setting.NotificationEmail
			if address == "" {
				address = userEmail
			}
			if address == "" {
				return NotificationDeliveryResult{NotificationUnconfigured, model.BalanceReminderNotConfigured}
			}
			err = common.SendEmailWithContext(ctx, data.Title, address, notificationContent(data))
		case dto.NotifyTypeWebhook:
			if setting.WebhookUrl == "" {
				return NotificationDeliveryResult{NotificationUnconfigured, model.BalanceReminderNotConfigured}
			}
			err = SendWebhookNotifyWithContext(ctx, setting.WebhookUrl, setting.WebhookSecret, data)
		case dto.NotifyTypeBark:
			if setting.BarkUrl == "" {
				return NotificationDeliveryResult{NotificationUnconfigured, model.BalanceReminderNotConfigured}
			}
			finalURL := strings.ReplaceAll(setting.BarkUrl, "{{title}}", url.QueryEscape(data.Title))
			finalURL = strings.ReplaceAll(finalURL, "{{content}}", url.QueryEscape(notificationContent(data)))
			err = sendNotificationRequest(ctx, useWorker, http.MethodGet, finalURL, map[string]string{"User-Agent": "OneAPI-Bark-Notify/1.0"}, nil)
		case dto.NotifyTypeGotify:
			if setting.GotifyUrl == "" || setting.GotifyToken == "" {
				return NotificationDeliveryResult{NotificationUnconfigured, model.BalanceReminderNotConfigured}
			}
			priority := setting.GotifyPriority
			if priority < 0 || priority > 10 {
				priority = 5
			}
			var body []byte
			body, err = common.Marshal(struct {
				Title    string `json:"title"`
				Message  string `json:"message"`
				Priority int    `json:"priority"`
			}{data.Title, notificationContent(data), priority})
			if err == nil {
				finalURL := strings.TrimSuffix(setting.GotifyUrl, "/") + "/message?token=" + url.QueryEscape(setting.GotifyToken)
				headers := map[string]string{"Content-Type": "application/json; charset=utf-8", "User-Agent": "NewAPI-Gotify-Notify/1.0"}
				if useWorker {
					headers["User-Agent"] = "OneAPI-Gotify-Notify/1.0"
				}
				err = sendNotificationRequest(ctx, useWorker, http.MethodPost, finalURL, headers, body)
			}
		default:
			return NotificationDeliveryResult{NotificationRejected, model.BalanceReminderInvalidDestination}
		}
	}
	if err == nil {
		return NotificationDeliveryResult{NotificationAccepted, model.BalanceReminderDelivered}
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return NotificationDeliveryResult{NotificationRetry, model.BalanceReminderCancelled}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return NotificationDeliveryResult{NotificationRetry, model.BalanceReminderTimeout}
	}
	if errors.Is(err, errNotificationDestination) {
		return NotificationDeliveryResult{NotificationRejected, model.BalanceReminderInvalidDestination}
	}
	status := 0
	var statusError *notificationHTTPStatusError
	if errors.As(err, &statusError) {
		status = statusError.status
	}
	var emailError *common.EmailDeliveryError
	if errors.As(err, &emailError) {
		if emailError.NotConfigured {
			return NotificationDeliveryResult{NotificationUnconfigured, model.BalanceReminderNotConfigured}
		}
		if emailError.InvalidRecipient {
			return NotificationDeliveryResult{NotificationRejected, model.BalanceReminderInvalidDestination}
		}
		status = emailError.HTTPStatus
	}
	var smtpError *textproto.Error
	if errors.As(err, &smtpError) && smtpError.Code >= 500 && smtpError.Code < 600 {
		return NotificationDeliveryResult{NotificationRejected, model.BalanceReminderTransportRejected}
	}
	if status >= 300 && status < 500 && status != 408 && status != 425 && status != 429 {
		return NotificationDeliveryResult{NotificationRejected, model.BalanceReminderTransportRejected}
	}
	return NotificationDeliveryResult{NotificationRetry, model.BalanceReminderDeliveryFailed}
}

// NotifyUser retains the existing limiter and nil-on-unconfigured contract for
// older notice callers. Durable delivery must use the explicit result above.
func NotifyUser(userId int, userEmail string, userSetting dto.UserSetting, data dto.Notify) error {
	canSend, err := CheckNotificationLimit(userId, data.Type)
	if err != nil {
		return errors.New("notification limit check failed")
	}
	if !canSend {
		return errors.New("notification limit exceeded")
	}
	result := DeliverUserNotification(context.Background(), userEmail, userSetting, data)
	if result.Status == NotificationAccepted || result.Status == NotificationUnconfigured {
		return nil
	}
	return errors.New(result.Code)
}

func notificationContent(data dto.Notify) string {
	content := data.Content
	for _, value := range data.Values {
		content = strings.Replace(content, dto.ContentValueParam, fmt.Sprintf("%v", value), 1)
	}
	return content
}

// sendNotificationRequest shares the existing Worker/SSRF policy across all
// notification transports, with real cancellation through DNS and network I/O.
func sendNotificationRequest(ctx context.Context, useWorker bool, method, destination string, headers map[string]string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	var response *http.Response
	var err error
	if useWorker {
		response, err = DoWorkerRequestWithContext(ctx, &WorkerRequest{
			URL: destination, Key: system_setting.WorkerValidKey, Method: method, Headers: headers, Body: body,
		})
	} else {
		if err = ValidateSSRFProtectedFetchURLWithContext(ctx, destination); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errNotificationDestination
		}
		var request *http.Request
		request, err = http.NewRequestWithContext(ctx, method, destination, bytes.NewReader(body))
		if err != nil {
			return errNotificationDestination
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err = GetSSRFProtectedHTTPClient().Do(request)
	}
	if err != nil {
		return &notificationTransportError{cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &notificationHTTPStatusError{status: response.StatusCode}
	}
	return nil
}
