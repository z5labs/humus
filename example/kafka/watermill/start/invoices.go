// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"encoding/json"
	"errors"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
)

type order struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Total    int    `json:"total"`
}

type invoice struct {
	OrderID  string `json:"order_id"`
	Customer string `json:"customer"`
	Amount   int    `json:"amount"`
}

func issueInvoice(msg *message.Message) ([]*message.Message, error) {
	var o order
	if err := json.Unmarshal(msg.Payload, &o); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, errors.New("the order has no id")
	}

	payload, err := json.Marshal(invoice{OrderID: o.ID, Customer: o.Customer, Amount: o.Total})
	if err != nil {
		return nil, err
	}
	return []*message.Message{message.NewMessage(watermill.NewUUID(), payload)}, nil
}
