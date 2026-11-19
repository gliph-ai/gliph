package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Outbound mail is one thing: the address-verification code. It goes through
// Pushleft's transactional mail (POST /v1/send with a MAIL_TOKEN), so there is
// no SMTP client or AWS signer here.
type mailer func(to, subject, text string) error

func pushleftMailer(token, from string) mailer {
	if token == "" {
		// Every send fails, so signup says the mail did not go instead of
		// leaving an account that can never be confirmed.
		log.Printf("warning: no MAIL_TOKEN; signups will fail until mail is configured")
		return func(to, subject, text string) error {
			return errors.New("this control plane cannot send mail (no MAIL_TOKEN)")
		}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	return func(to, subject, text string) error {
		body, _ := json.Marshal(map[string]any{"from": from, "to": []string{to}, "subject": subject, "text": text})
		req, err := http.NewRequest("POST", "https://mail.pushleft.dev/v1/send", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
			return fmt.Errorf("mail: HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(detail)))
		}
		return nil
	}
}
