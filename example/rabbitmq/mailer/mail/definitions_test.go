// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package mail_test

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/rabbitmq/mailer/mail"
)

type definitions struct {
	Users       []json.RawMessage `json:"users"`
	Permissions []json.RawMessage `json:"permissions"`
	Exchanges   []struct {
		Name       string         `json:"name"`
		VHost      string         `json:"vhost"`
		Type       string         `json:"type"`
		Durable    bool           `json:"durable"`
		AutoDelete bool           `json:"auto_delete"`
		Internal   bool           `json:"internal"`
		Arguments  map[string]any `json:"arguments"`
	} `json:"exchanges"`
	Queues []struct {
		Name       string         `json:"name"`
		VHost      string         `json:"vhost"`
		Durable    bool           `json:"durable"`
		AutoDelete bool           `json:"auto_delete"`
		Arguments  map[string]any `json:"arguments"`
	} `json:"queues"`
	Bindings []struct {
		Source          string         `json:"source"`
		VHost           string         `json:"vhost"`
		Destination     string         `json:"destination"`
		DestinationType string         `json:"destination_type"`
		RoutingKey      string         `json:"routing_key"`
		Arguments       map[string]any `json:"arguments"`
	} `json:"bindings"`
	Policies []struct {
		Name       string         `json:"name"`
		VHost      string         `json:"vhost"`
		Pattern    string         `json:"pattern"`
		ApplyTo    string         `json:"apply-to"`
		Priority   int            `json:"priority"`
		Definition map[string]any `json:"definition"`
	} `json:"policies"`
}

func readDefinitions(t *testing.T) (definitions, map[string]any) {
	t.Helper()
	b, err := os.ReadFile("../deploy/rabbitmq/definitions.json")
	require.NoError(t, err)
	var d definitions
	require.NoError(t, json.Unmarshal(b, &d))
	var raw map[string]any
	require.NoError(t, json.Unmarshal(b, &raw))
	return d, raw
}

func TestDefinitions_hold_no_logins(t *testing.T) {
	d, raw := readDefinitions(t)

	require.Empty(t, d.Users)
	require.Empty(t, d.Permissions)
	require.ElementsMatch(t, []string{"exchanges", "queues", "bindings", "policies"}, slices.Collect(maps.Keys(raw)))
}

func TestDefinitions_declare_every_exchange(t *testing.T) {
	d, _ := readDefinitions(t)

	types := map[string]string{}
	for _, e := range d.Exchanges {
		require.Equal(t, "/", e.VHost, e.Name)
		require.True(t, e.Durable, e.Name)
		require.False(t, e.AutoDelete, e.Name)
		require.False(t, e.Internal, e.Name)
		require.Empty(t, e.Arguments, e.Name)
		types[e.Name] = e.Type
	}
	require.Equal(t, map[string]string{
		mail.Exchange:       "direct",
		mail.RetryExchange:  "direct",
		mail.DeadExchange:   "fanout",
		mail.EventsExchange: "topic",
	}, types)
}

func TestDefinitions_declare_every_queue_as_a_quorum_queue(t *testing.T) {
	d, _ := readDefinitions(t)

	var names []string
	for _, q := range d.Queues {
		require.Equal(t, "/", q.VHost, q.Name)
		require.True(t, q.Durable, q.Name)
		require.False(t, q.AutoDelete, q.Name)
		require.Equal(t, map[string]any{"x-queue-type": "quorum"}, q.Arguments, q.Name)
		names = append(names, q.Name)
	}
	require.ElementsMatch(t, []string{mail.SendQueue, mail.RetryQueue, mail.DeadQueue}, names)
}

func TestDefinitions_bind_every_queue(t *testing.T) {
	d, _ := readDefinitions(t)

	type binding struct{ source, destination, key string }
	var bindings []binding
	for _, b := range d.Bindings {
		require.Equal(t, "/", b.VHost, b.Destination)
		require.Equal(t, "queue", b.DestinationType, b.Destination)
		require.Empty(t, b.Arguments, b.Destination)
		bindings = append(bindings, binding{b.Source, b.Destination, b.RoutingKey})
	}
	require.ElementsMatch(t, []binding{
		{mail.Exchange, mail.SendQueue, mail.SendKey},
		{mail.RetryExchange, mail.RetryQueue, mail.SendKey},
		{mail.DeadExchange, mail.DeadQueue, ""},
	}, bindings)
}

func TestDefinitions_set_the_queues_behaviour_in_policies(t *testing.T) {
	d, _ := readDefinitions(t)

	type policy struct {
		pattern    string
		definition map[string]any
	}
	policies := map[string]policy{}
	for _, p := range d.Policies {
		require.Equal(t, "/", p.VHost, p.Name)
		require.Equal(t, "queues", p.ApplyTo, p.Name)
		require.Zero(t, p.Priority, p.Name)
		policies[p.Name] = policy{p.Pattern, p.Definition}
	}
	require.Equal(t, map[string]policy{
		mail.SendQueue: {
			pattern: `^mail\.send$`,
			definition: map[string]any{
				"delivery-limit":       float64(5),
				"dead-letter-exchange": mail.DeadExchange,
				"dead-letter-strategy": "at-least-once",
				"overflow":             "reject-publish",
			},
		},
		mail.RetryQueue: {
			pattern: `^mail\.send\.retry$`,
			definition: map[string]any{
				"message-ttl":             float64(5 * 60 * 1000),
				"dead-letter-exchange":    mail.Exchange,
				"dead-letter-routing-key": mail.SendKey,
				"dead-letter-strategy":    "at-least-once",
				"overflow":                "reject-publish",
			},
		},
	}, policies)
}
