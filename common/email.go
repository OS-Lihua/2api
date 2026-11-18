package common

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"slices"
	"strings"
	"time"
)

// EmailDeliveryError preserves the safe public message while allowing durable
// notification delivery to distinguish configuration, recipient and HTTP errors.
type EmailDeliveryError struct {
	Cause            error
	NotConfigured    bool
	InvalidRecipient bool
	HTTPStatus       int
}

func (err *EmailDeliveryError) Error() string { return err.Cause.Error() }
func (err *EmailDeliveryError) Unwrap() error { return err.Cause }

func generateMessageID() (string, error) {
	split := strings.Split(SMTPFrom, "@")
	if len(split) < 2 {
		return "", fmt.Errorf("invalid SMTP account")
	}
	domain := strings.Split(SMTPFrom, "@")[1]
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), GetRandomString(12), domain), nil
}

func shouldUseSMTPLoginAuth() bool {
	if SMTPForceAuthLogin {
		return true
	}
	return isOutlookServer(SMTPAccount) || slices.Contains(EmailLoginAuthServerList, SMTPServer)
}

func getSMTPAuth() smtp.Auth {
	return AutoSMTPAuth(SMTPAccount, SMTPToken)
}

func shouldAuthenticateSMTP() bool {
	return SMTPAccount != "" && SMTPToken != ""
}

func smtpTLSConfig() *tls.Config {
	return &tls.Config{
		ServerName:         SMTPServer,
		InsecureSkipVerify: SMTPInsecureSkipVerify, // #nosec G402 -- admin-controlled SMTP compatibility option.
	}
}

type smtpContextConn struct {
	net.Conn
	stopCancellation func() bool
}

func (conn *smtpContextConn) Close() error {
	conn.stopCancellation()
	return conn.Conn.Close()
}

func newSMTPClient(addr string) (*smtp.Client, error) {
	return newSMTPClientWithContext(context.Background(), addr)
}

func newSMTPClientWithContext(ctx context.Context, addr string) (*smtp.Client, error) {
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := raw.SetDeadline(deadline); err != nil {
			_ = raw.Close()
			return nil, err
		}
	}
	conn := &smtpContextConn{Conn: raw}
	conn.stopCancellation = context.AfterFunc(ctx, func() { _ = raw.Close() })
	var transport net.Conn = conn
	implicitTLS := SMTPSSLEnabled || (SMTPPort == 465 && !SMTPStartTLSEnabled)
	if implicitTLS {
		transport = tls.Client(conn, smtpTLSConfig())
	}
	client, err := smtp.NewClient(transport, SMTPServer)
	if err != nil {
		_ = transport.Close()
		return nil, err
	}

	if !implicitTLS && SMTPStartTLSEnabled {
		startTLSSupported, _ := client.Extension("STARTTLS")
		if !startTLSSupported {
			_ = client.Close()
			return nil, fmt.Errorf("SMTP server does not support STARTTLS")
		}
		if err := client.StartTLS(smtpTLSConfig()); err != nil {
			_ = client.Close()
			return nil, err
		}
	}

	return client, nil
}

func SendEmail(subject string, receiver string, content string) error {
	return SendEmailWithContext(context.Background(), subject, receiver, content)
}

// SendEmailWithContext bounds the entire selected delivery attempt, including
// SMTP greeting, TLS negotiation and message acknowledgement. Cancellation
// closes the underlying connection instead of abandoning a sending goroutine.
func SendEmailWithContext(ctx context.Context, subject string, receiver string, content string) (sendErr error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	defer func() {
		if sendErr == nil {
			return
		}
		if ctx.Err() != nil {
			sendErr = ctx.Err()
		} else if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			// The socket deadline can wake before the context's timer callback.
			sendErr = context.DeadlineExceeded
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	OptionMapRWMutex.RLock()
	provider := OptionMap["EmailProvider"]
	resendKey := OptionMap["ResendApiKey"]
	resendFrom := OptionMap["ResendFrom"]
	OptionMapRWMutex.RUnlock()
	// One selector controls delivery. Never fall back to a second provider:
	// a failed response does not prove that the first provider did not send.
	switch provider {
	case "resend":
		return sendResendEmail(ctx, resendKey, resendFrom, subject, receiver, content)
	case "", "smtp": // Preserve existing installations without a selector.
	default:
		return NewMessage("Email sending method must be SMTP or Resend")
	}
	if SMTPFrom == "" { // for compatibility
		SMTPFrom = SMTPAccount
	}
	if SMTPServer == "" && SMTPAccount == "" {
		return &EmailDeliveryError{Cause: NewMessage("SMTP server is not configured"), NotConfigured: true}
	}
	id, err2 := generateMessageID()
	if err2 != nil {
		return &EmailDeliveryError{Cause: err2, NotConfigured: true}
	}
	encodedSubject := fmt.Sprintf("=?UTF-8?B?%s?=", base64.StdEncoding.EncodeToString([]byte(subject)))
	mail := []byte(fmt.Sprintf("To: %s\r\n"+
		"From: %s <%s>\r\n"+
		"Subject: %s\r\n"+
		"Date: %s\r\n"+
		"Message-ID: %s\r\n"+ // 添加 Message-ID 头
		"Content-Type: text/html; charset=UTF-8\r\n\r\n%s\r\n",
		receiver, SystemName, SMTPFrom, encodedSubject, time.Now().Format(time.RFC1123Z), id, content))
	auth := getSMTPAuth()
	addr := fmt.Sprintf("%s:%d", SMTPServer, SMTPPort)
	to := strings.Split(receiver, ";")
	var err error
	client, err := newSMTPClientWithContext(ctx, addr)
	if err != nil {
		return err
	}
	defer client.Close()
	if shouldAuthenticateSMTP() {
		if err = client.Auth(auth); err != nil {
			return err
		}
	}
	if err = client.Mail(SMTPFrom); err != nil {
		return err
	}
	for _, receiver := range to {
		if err = client.Rcpt(receiver); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	_, err = w.Write(mail)
	if err != nil {
		return err
	}
	err = w.Close()
	if err != nil {
		return err
	}
	// DATA's positive completion reply already confirms provider acceptance.
	// A later QUIT failure must not turn that receipt into a retry or leak the
	// recipient/provider's arbitrary reply through server diagnostics.
	if err = client.Quit(); err != nil {
		SysError(LogText("SMTP session close failed after message acceptance"))
	}
	return nil
}
