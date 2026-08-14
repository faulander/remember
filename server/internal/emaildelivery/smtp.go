// Package emaildelivery delivers durable identity verification messages.
package emaildelivery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

const verificationTokenBytes = 32

// Sender delivers one email-verification token. Implementations must be safe
// for sequential reuse by the dispatcher.
type Sender interface {
	SendVerification(context.Context, string, string) error
}

// SMTPConfig describes an implicit-TLS SMTP submission endpoint.
type SMTPConfig struct {
	Address  string
	Username string
	Password string
	From     string
	Timeout  time.Duration
}

type SMTPSender struct {
	address, host, username, password, from string
	timeout                                 time.Duration
	rootCAs                                 *x509.CertPool
}

func NewSMTPSender(config SMTPConfig) (*SMTPSender, error) {
	host, port, err := net.SplitHostPort(config.Address)
	if err != nil || host == "" || port == "" {
		return nil, errors.New("invalid SMTP address")
	}
	if config.Username == "" || config.Password == "" || strings.ContainsAny(config.Username, "\r\n\x00") || strings.ContainsAny(config.Password, "\x00") {
		return nil, errors.New("invalid SMTP credentials")
	}
	from, err := mail.ParseAddress(config.From)
	if err != nil || from.Address != config.From || strings.ContainsAny(config.From, "\r\n") {
		return nil, errors.New("invalid SMTP sender")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("invalid SMTP timeout")
	}
	return &SMTPSender{address: config.Address, host: host, username: config.Username, password: config.Password, from: from.Address, timeout: config.Timeout}, nil
}

func (s *SMTPSender) SendVerification(ctx context.Context, recipient, token string) error {
	address, err := mail.ParseAddress(recipient)
	if err != nil || address.Address != recipient || strings.ContainsAny(recipient, "\r\n") || !validVerificationToken(token) {
		return errors.New("invalid verification message")
	}
	dialer := &tls.Dialer{Config: &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12, RootCAs: s.rootCAs}}
	connection, err := dialer.DialContext(ctx, "tcp", s.address)
	if err != nil {
		return errors.New("connect verification delivery")
	}
	defer connection.Close()
	deadline := time.Now().Add(s.timeout)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return errors.New("bound verification delivery")
	}
	client, err := smtp.NewClient(connection, s.host)
	if err != nil {
		return errors.New("open verification delivery")
	}
	defer client.Close()
	if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
		return errors.New("authenticate verification delivery")
	}
	if err := client.Mail(s.from); err != nil {
		return errors.New("set verification sender")
	}
	if err := client.Rcpt(address.Address); err != nil {
		return errors.New("set verification recipient")
	}
	body, err := client.Data()
	if err != nil {
		return errors.New("start verification message")
	}
	message := "From: " + s.from + "\r\nTo: " + address.Address + "\r\nSubject: Remember email verification\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\nGib diesen Verifizierungscode in Remember ein:\r\n\r\n" + token + "\r\n\r\nDer Code ist 24 Stunden gueltig.\r\n"
	if _, err := io.WriteString(body, message); err != nil {
		_ = body.Close()
		return errors.New("write verification message")
	}
	if err := body.Close(); err != nil {
		return errors.New("finish verification message")
	}
	if err := client.Quit(); err != nil {
		return errors.New("close verification delivery")
	}
	return nil
}

func validVerificationToken(token string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(raw) == verificationTokenBytes && len(token) == 43
}
