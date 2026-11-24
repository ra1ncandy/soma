package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// EmailConfig configures SMTP outbound delivery for soma bridge notifications and replies.
type EmailConfig struct {
	SMTPHost string
	SMTPPort int
	Username string
	Password string
	From     string
	Subject  string
}

// EmailAdapter delivers OutboundMessages over SMTP.
type EmailAdapter struct {
	Config   EmailConfig
	sendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// NewEmailAdapter creates an EmailAdapter using net/smtp.SendMail.
func NewEmailAdapter(cfg EmailConfig) *EmailAdapter {
	if cfg.SMTPPort <= 0 {
		cfg.SMTPPort = 587
	}
	if strings.TrimSpace(cfg.Subject) == "" {
		cfg.Subject = "[soma] Notification"
	}
	return &EmailAdapter{
		Config:   cfg,
		sendMail: smtp.SendMail,
	}
}

// Name returns "email".
func (a *EmailAdapter) Name() string { return "email" }

// Send delivers msg.Text via SMTP to msg.Target (comma-separated recipient addresses).
func (a *EmailAdapter) Send(_ context.Context, msg OutboundMessage) error {
	if strings.TrimSpace(a.Config.SMTPHost) == "" {
		return errors.New("email: SMTPHost is required")
	}
	if strings.TrimSpace(a.Config.From) == "" {
		return errors.New("email: From address is required")
	}
	target := strings.TrimSpace(msg.Target)
	if target == "" {
		return errors.New("email: recipient Target is required")
	}
	var to []string
	for _, r := range strings.Split(target, ",") {
		if t := strings.TrimSpace(r); t != "" {
			to = append(to, t)
		}
	}
	if len(to) == 0 {
		return errors.New("email: no valid recipients")
	}

	addr := net.JoinHostPort(a.Config.SMTPHost, fmt.Sprintf("%d", a.Config.SMTPPort))
	var auth smtp.Auth
	if a.Config.Username != "" {
		auth = smtp.PlainAuth("", a.Config.Username, a.Config.Password, a.Config.SMTPHost)
	}
	payload := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n",
		a.Config.From, strings.Join(to, ", "), a.Config.Subject, msg.Text)

	fn := a.sendMail
	if fn == nil {
		fn = smtp.SendMail
	}
	return fn(addr, auth, a.Config.From, to, []byte(payload))
}
