package emaildelivery

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSMTPSenderRejectsUnsafeConfigurationAndMessages(t *testing.T) {
	t.Parallel()
	valid := SMTPConfig{Address: "smtp.example.com:465", Username: "remember", Password: "secret", From: "remember@example.com", Timeout: time.Second}
	for name, mutate := range map[string]func(*SMTPConfig){
		"address":  func(config *SMTPConfig) { config.Address = "smtp.example.com" },
		"username": func(config *SMTPConfig) { config.Username = "user\nname" },
		"password": func(config *SMTPConfig) { config.Password = "secret\x00value" },
		"sender":   func(config *SMTPConfig) { config.From = "Remember <remember@example.com>" },
		"timeout":  func(config *SMTPConfig) { config.Timeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := NewSMTPSender(config); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("NewSMTPSender() error = %v", err)
			}
		})
	}
	sender, err := NewSMTPSender(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.SendVerification(context.Background(), "person@example.com\nBcc:x@example.com", strings.Repeat("A", 43)); err == nil {
		t.Fatal("SendVerification accepted header injection")
	}
	if err := sender.SendVerification(context.Background(), "person@example.com", "not-a-token"); err == nil {
		t.Fatal("SendVerification accepted malformed token")
	}
}

func TestSMTPSenderDeliversOverVerifiedTLS(t *testing.T) {
	t.Parallel()
	certificateSource := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := certificateSource.TLS.Certificates[0]
	root := certificateSource.Certificate()
	certificateSource.Close()

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	delivered := make(chan string, 1)
	serverErrors := make(chan error, 1)
	go func() {
		message, err := serveSMTP(listener)
		if err != nil {
			serverErrors <- err
			return
		}
		delivered <- message
	}()

	sender, err := NewSMTPSender(SMTPConfig{
		Address: listener.Addr().String(), Username: "remember", Password: "secret",
		From: "remember@example.com", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	sender.rootCAs = roots
	token := strings.Repeat("A", 43)
	if err := sender.SendVerification(context.Background(), "person@example.com", token); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverErrors:
		t.Fatal(err)
	case message := <-delivered:
		if !strings.Contains(message, "To: person@example.com\r\n") || !strings.Contains(message, "\r\n\r\n"+token+"\r\n") {
			t.Fatalf("delivered message missing recipient or token: %q", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SMTP server did not finish")
	}
}

func serveSMTP(listener net.Listener) (string, error) {
	connection, err := listener.Accept()
	if err != nil {
		return "", err
	}
	defer connection.Close()
	reader, writer := bufio.NewReader(connection), bufio.NewWriter(connection)
	write := func(response string) error {
		if _, err := writer.WriteString(response); err != nil {
			return err
		}
		return writer.Flush()
	}
	read := func(prefix string) error {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasPrefix(line, prefix) {
			return fmt.Errorf("SMTP command %q does not start with %q", line, prefix)
		}
		return nil
	}
	if err := write("220 localhost ESMTP\r\n"); err != nil {
		return "", err
	}
	if err := read("EHLO "); err != nil {
		return "", err
	}
	if err := write("250-localhost\r\n250 AUTH PLAIN\r\n"); err != nil {
		return "", err
	}
	for _, exchange := range []struct {
		command, response string
	}{
		{"AUTH PLAIN ", "235 2.7.0 authenticated\r\n"},
		{"MAIL FROM:", "250 2.1.0 sender ok\r\n"},
		{"RCPT TO:", "250 2.1.5 recipient ok\r\n"},
		{"DATA", "354 end with dot\r\n"},
	} {
		if err := read(exchange.command); err != nil {
			return "", err
		}
		if err := write(exchange.response); err != nil {
			return "", err
		}
	}
	var message strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		if line == ".\r\n" {
			break
		}
		message.WriteString(line)
	}
	if err := write("250 2.0.0 queued\r\n"); err != nil {
		return "", err
	}
	if err := read("QUIT"); err != nil {
		return "", err
	}
	if err := write("221 2.0.0 bye\r\n"); err != nil {
		return "", err
	}
	return message.String(), nil
}
