# Calling the provider

The provider is invented for the walkthrough, and is one endpoint:
`POST /v1/messages`, with a bearer key, an `Idempotency-Key`, and the
sender, recipient, subject and text as JSON. It answers 2xx with the
email's `id`, or a 4xx with a `reason`.

## Its settings and its result

Start the `provider` package with what `Init` hands it, and what a send
hands back:

```go
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
```

## The client

`New` checks the settings, then builds an `httpclient` client for the
provider's URL, trusting `CA`, or the system's roots when it's nil:

```go
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
```

The API key is set once, as the `Authorization` header's value. `Init`
reads it from a file.

## `Classify`

The default `Classify`, plus one answer. The provider answers 409 while a
request with the same key is in flight, such as when a redelivery reaches a
second replica, so a 409 is retried and counts for the provider, not against
it:

```go
func withConflict(classify func(*http.Response, error) httpclient.Classification) func(*http.Response, error) httpclient.Classification {
	return func(resp *http.Response, err error) httpclient.Classification {
		if resp != nil && resp.StatusCode == http.StatusConflict {
			return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
		}
		return classify(resp, err)
	}
}
```

## `Retry-After`

The default `Classify` waits out a 429's or a 503's `Retry-After`. A
`Retry-After` longer than `BackoffMax` ends the call instead, and the
request waits in the delay queue, so a rate limit holds no worker.

## The request and its key

The body and the two answers, as JSON:

```go
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
```

The request's `MessageId` is its `Idempotency-Key`:

```go
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
```

Retrying a `POST` is safe only because it carries that key. The body is a
`*bytes.Reader`, so `httpclient` can send it again.

## The answers

The client sends it, then reads at most 64 KiB of the answer:

```go
resp, err := c.http.Do(req)
if err != nil {
	return Result{}, fmt.Errorf("provider: %w", err)
}
defer func() { _ = resp.Body.Close() }()
answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerSize))
if err != nil {
	return Result{}, fmt.Errorf("provider: reading its %s answer: %w", resp.Status, err)
}
```

```go
const maxAnswerSize = 64 << 10
```

Each answer becomes a result, or an error:

```go
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
```

| The answer, after any retries | `Send` returns |
|---|---|
| 2xx with an `id` | the `id` |
| a 4xx other than 408, 409 and 429, with a `reason` | `Refused`, with the `reason` |
| anything else, a transport error, or the breaker open | an error |

An error is a failure for now, and the request is retried later.

## WireMock's mappings

WireMock stands in for the provider, in the kind overlay and the container
tests, with three mappings in `deploy/kind/wiremock/`. The lowest
`priority` is matched first.

No bearer token is a 401:

```json
{
  "priority": 1,
  "request": {
    "method": "POST",
    "url": "/v1/messages",
    "headers": {
      "Authorization": {
        "or": [
          { "absent": true },
          { "doesNotMatch": "^Bearer \\S+$" }
        ]
      }
    }
  },
  "response": {
    "status": 401,
    "headers": {
      "Content-Type": "application/json"
    },
    "jsonBody": {
      "reason": "no bearer token"
    }
  }
}
```

An address in `.invalid`, a domain that can't exist, is refused:

```json
{
  "priority": 2,
  "request": {
    "method": "POST",
    "url": "/v1/messages",
    "bodyPatterns": [
      {
        "matchesJsonPath": {
          "expression": "$.to",
          "matches": "(?i)^[^@]+@([^@.]+\\.)*invalid$"
        }
      }
    ]
  },
  "response": {
    "status": 422,
    "headers": {
      "Content-Type": "application/json"
    },
    "jsonBody": {
      "reason": "the recipient's domain does not exist"
    }
  }
}
```

Every other request is accepted, with an ID made from its
`Idempotency-Key`, so the same key always gets the same ID:

```json
{
  "priority": 3,
  "request": {
    "method": "POST",
    "url": "/v1/messages"
  },
  "response": {
    "status": 202,
    "headers": {
      "Content-Type": "application/json"
    },
    "body": "{\"id\":\"msg_{{request.headers.Idempotency-Key}}\"}",
    "transformers": ["response-template"]
  }
}
```

Next: [Processing a request](/humus/pr-preview/pr-1308/walkthroughs/rabbitmq/mailer/processing/)
