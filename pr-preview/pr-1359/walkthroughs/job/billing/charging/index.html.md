# Charging

The payments API is invented for the walkthrough, and is one endpoint:
`POST /v1/charges`, with a bearer key, an `Idempotency-Key`, and the account,
amount and currency as JSON.

## A charge and its result

Start the `payments` package with what the run hands it, and what it hands
back:

```go
type Charge struct {
	ID          uuid.UUID
	AccountID   string
	AmountCents int64
	Currency    string
}
```

```go
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
```

## Classify

`httpclient`'s default `Classify`, plus two answers. A 409 means a charge with
this key is still in flight, so it is retried, and every 5xx counts against
the breaker:

```go
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
```

## The client

`NewClient` builds an `httpclient` client with that `Classify`.
`ForwardScenario` is false, so a failure-injection scenario never reaches the
payments API:

```go
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
```

## The request and its key

The body and the answer, as JSON:

```go
type charge struct {
	Account  string `json:"account"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type answer struct {
	ID   string `json:"id"`
	Code string `json:"code"`
}
```

The request carries the key, and the invoice's ID as its `Idempotency-Key`:

```go
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
```

Retrying a `POST` is safe only because it carries that key. The body is a
`*bytes.Reader`, so `httpclient` can send it again.

## The answers

The client sends it, then reads at most 64 KiB of the answer:

```go
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
```

Each answer becomes a result, or an error that leaves the invoice pending:

```go
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
```

| The answer | The invoice |
|---|---|
| 201, or 200 with the charge already made for the key | `charged`, with its ID |
| 402 | `declined`, with its code |
| 401, 403 | still pending, and the run stops |
| 409, 429, 5xx or a transport error, after any retries | still pending |
| any other answer | `failed`, with the answer, for a person to look at |
| rejected by the open breaker | still pending, and the run stops |

`ErrStop` marks the errors that stop the run, because every further charge
would fail too:

```go
const maxAnswer = 64 << 10

var ErrStop = errors.New("every further charge would fail too")
```

```go
func answerOf(resp *http.Response, body []byte) string {
	b := strings.TrimSpace(string(body))
	if b == "" {
		return resp.Status
	}
	return resp.Status + ": " + b
}
```

Next: [The run](/humus/pr-preview/pr-1359/walkthroughs/job/billing/run/)
