// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package provider sends the mailer's emails through the provider's API.
package provider

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/rabbitmq/mailer/mail"
	"github.com/z5labs/humus/httpclient"
)

const maxAnswerSize = 64 << 10

type Settings struct {
	URL            *url.URL
	CA             *x509.CertPool
	Timeout        time.Duration
	AttemptTimeout time.Duration
	MaxAttempts    int
	From           string
	APIKey         string
}

type Result struct {
	Refused bool
	ID      string
	Reason  string
}

type Client struct {
	http          *http.Client
	messages      string
	from          string
	authorization string
}

func New(s *humus.Setup, settings Settings) (*Client, error) {
	var errs []error
	if settings.From == "" {
		errs = append(errs, errors.New("Settings.From must not be empty"))
	}
	if settings.APIKey == "" {
		errs = append(errs, errors.New("Settings.APIKey must not be empty"))
	}
	plain := settings.URL != nil && settings.URL.Scheme == "http"
	if plain && settings.CA != nil {
		errs = append(errs, errors.New("Settings.CA must be nil for an http Settings.URL, which is plain text"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("provider: New: %w", err)
	}

	h := httpclient.DefaultSettings()
	h.BaseURL = settings.URL
	if !plain {
		h.TLS = &tls.Config{RootCAs: settings.CA}
	}
	h.Timeout = settings.Timeout
	h.AttemptTimeout = settings.AttemptTimeout
	h.MaxAttempts = settings.MaxAttempts
	h.Classify = withConflict(h.Classify)
	c, err := httpclient.NewClient(s, h)
	if err != nil {
		return nil, fmt.Errorf("provider: New: %w", err)
	}
	return &Client{
		http:          c,
		messages:      settings.URL.JoinPath("v1", "messages").String(),
		from:          settings.From,
		authorization: "Bearer " + settings.APIKey,
	}, nil
}

func withConflict(classify func(*http.Response, error) httpclient.Classification) func(*http.Response, error) httpclient.Classification {
	return func(resp *http.Response, err error) httpclient.Classification {
		if resp != nil && resp.StatusCode == http.StatusConflict {
			return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
		}
		return classify(resp, err)
	}
}

type message struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

type accepted struct {
	ID string `json:"id"`
}

type refused struct {
	Reason string `json:"reason"`
}

func (c *Client) Send(ctx context.Context, r mail.Request) (Result, error) {
	b, err := json.Marshal(message{From: c.from, To: r.To, Subject: r.Subject, Text: r.Text})
	if err != nil {
		return Result{}, fmt.Errorf("provider: encoding the message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.messages, bytes.NewReader(b))
	if err != nil {
		return Result{}, fmt.Errorf("provider: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", r.MessageID)
	req.Header.Set("Authorization", c.authorization)

	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("provider: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerSize))
	if err != nil {
		return Result{}, fmt.Errorf("provider: reading its %s answer: %w", resp.Status, err)
	}

	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		var a accepted
		if err := json.Unmarshal(answer, &a); err != nil || a.ID == "" {
			return Result{}, fmt.Errorf("provider: it answered %s without an id", resp.Status)
		}
		return Result{ID: a.ID}, nil
	case code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusConflict && code != http.StatusTooManyRequests:
		var a refused
		if err := json.Unmarshal(answer, &a); err != nil || a.Reason == "" {
			return Result{}, fmt.Errorf("provider: it answered %s without a reason", resp.Status)
		}
		return Result{Refused: true, Reason: a.Reason}, nil
	default:
		return Result{}, fmt.Errorf("provider: it answered %s", resp.Status)
	}
}
