// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package payments charges the billing job's invoices through the payments API.
package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
)

const maxAnswer = 64 << 10

var ErrStop = errors.New("every further charge would fail too")

type Charge struct {
	ID          uuid.UUID
	AccountID   string
	AmountCents int64
	Currency    string
}

type State int

const (
	Charged State = iota + 1
	Declined
	Failed
)

type Result struct {
	State       State
	ChargeID    string
	DeclineCode string
	Answer      string
}

var defaultClassify = httpclient.DefaultSettings().Classify

func Classify(resp *http.Response, err error) httpclient.Classification {
	if err != nil {
		return defaultClassify(resp, err)
	}
	if resp.StatusCode == http.StatusConflict {
		return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
	}
	c := defaultClassify(resp, err)
	if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
		c.Breaker = circuit.Failure
	}
	return c
}

type Client struct {
	http    *http.Client
	charges string
	key     string
}

func NewClient(s *humus.Setup, settings httpclient.Settings, key string) (*Client, error) {
	settings.Classify = Classify
	settings.ForwardScenario = false
	c, err := httpclient.NewClient(s, settings)
	if err != nil {
		return nil, err
	}
	return &Client{
		http:    c,
		charges: settings.BaseURL.JoinPath("v1", "charges").String(),
		key:     key,
	}, nil
}

type charge struct {
	Account  string `json:"account"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type answer struct {
	ID   string `json:"id"`
	Code string `json:"code"`
}

func (c *Client) Charge(ctx context.Context, ch Charge) (Result, error) {
	body, err := json.Marshal(charge{Account: ch.AccountID, Amount: ch.AmountCents, Currency: ch.Currency})
	if err != nil {
		return Result{}, fmt.Errorf("charging invoice %s: %w", ch.ID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.charges, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("charging invoice %s: %w", ch.ID, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Idempotency-Key", ch.ID.String())
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if errors.Is(err, circuit.ErrRejected) {
		return Result{}, fmt.Errorf("charging invoice %s: %w: %w", ch.ID, ErrStop, err)
	}
	if err != nil {
		return Result{}, fmt.Errorf("charging invoice %s: %w", ch.ID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return Result{}, fmt.Errorf("charging invoice %s: reading the answer to %s: %w", ch.ID, resp.Status, err)
	}

	switch code := resp.StatusCode; {
	case code == http.StatusCreated, code == http.StatusOK:
		var a answer
		if err := json.Unmarshal(b, &a); err != nil || a.ID == "" {
			return Result{State: Failed, Answer: answerOf(resp, b)}, nil
		}
		return Result{State: Charged, ChargeID: a.ID}, nil
	case code == http.StatusPaymentRequired:
		var a answer
		_ = json.Unmarshal(b, &a)
		return Result{State: Declined, DeclineCode: a.Code}, nil
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return Result{}, fmt.Errorf("charging invoice %s: %w: the payments API rejected the key: %s", ch.ID, ErrStop, answerOf(resp, b))
	case code == http.StatusConflict, code == http.StatusTooManyRequests, code >= 500:
		return Result{}, fmt.Errorf("charging invoice %s: the payments API answered %s", ch.ID, answerOf(resp, b))
	default:
		return Result{State: Failed, Answer: answerOf(resp, b)}, nil
	}
}

func answerOf(resp *http.Response, body []byte) string {
	b := strings.TrimSpace(string(body))
	if b == "" {
		return resp.Status
	}
	return resp.Status + ": " + b
}
