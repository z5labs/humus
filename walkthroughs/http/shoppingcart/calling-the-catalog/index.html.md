# Calling the catalog

The cart has one `httpclient` client for the catalog. Prices and readiness
both go through it.

## The client's settings

The cart's `readSettings` starts from `httpclient.DefaultSettings()`, and
sets the catalog's URL, the CA it trusts the catalog through, and the
client certificate it presents:

```go
catalog := httpclient.DefaultSettings()
catalog.BaseURL = r.Get(env("CATALOG_URL").Parse(url.Parse).Validate(checkCatalogURL))
catalog.TLS = &tls.Config{
	RootCAs: r.Get(file("CATALOG_CA_FILE").Parse(certPool)),
	GetClientCertificate: r.Get(env("CATALOG_CLIENT_CERT_FILE").Validate(holdsCertificate(s.Host.FS)).FlatMap(func(certFile string) config.Value[clientCertificate] {
		return env("CATALOG_CLIENT_KEY_FILE").Parse(func(keyFile string) (clientCertificate, error) {
			cfg, err := certfile.Config(s.Host.FS, certFile, keyFile)
			if err != nil {
				return nil, err
			}
			return cfg.GetClientCertificate, nil
		})
	})),
}
catalog.Timeout = r.Get(env("CATALOG_TIMEOUT").Parse(time.ParseDuration).Default(catalog.Timeout).Validate(httpclient.CheckTimeout))
catalog.AttemptTimeout = r.Get(env("CATALOG_ATTEMPT_TIMEOUT").Parse(time.ParseDuration).Default(catalog.AttemptTimeout).Validate(httpclient.CheckAttemptTimeout))
catalog.MaxAttempts = r.Get(env("CATALOG_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(catalog.MaxAttempts).Validate(httpclient.CheckMaxAttempts))
catalog.ForwardScenario = true
```

```go
type clientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
```

The catalog asks for a client certificate, so its URL must be `https`:

```go
func checkCatalogURL(u *url.URL) error {
	if err := httpclient.CheckBaseURL(u); err != nil {
		return err
	}
	if u.Scheme != "https" {
		return errors.New("must be an https URL")
	}
	return nil
}
```

## Retries and the breaker

The cart keeps the default `Classify`, retry budget and breaker. A price is
a `GET`, so it is safe to retry:

| The catalog's answer | Retried | To the breaker |
|---|---|---|
| 200, or a 404 | no | a success |
| 502, 503 or 504 | yes | a failure |
| a refused or reset connection, or an `AttemptTimeout` | yes | a failure |
| a 500 | no | a failure |

A 404 is the catalog working, so it counts for the catalog, not against it.
[Calling HTTP services](/humus/dependencies/http/#how-an-attempts-result-counts)
describes the default.

## `ForwardScenario`

`ForwardScenario` sends a call's failure-injection scenario on to the
catalog, because the catalog is the walkthrough's own and reads the same
codec. So a scenario the cart trusts can fail the catalog's
`postgresql:catalog` point too, and the catalog trusts it because the
cart's client certificate verified.

## The client and the Catalog

`openCatalog` builds the client, and the two things the cart needs from
it, a `Catalog` and a readiness check:

```go
func openCatalog(s *humus.Setup, settings httpclient.Settings) (endpoint.Catalog, func(context.Context) error, error) {
	client, err := httpclient.NewClient(s, settings)
	if err != nil {
		return nil, nil, err
	}
	return catalogclient.New(client, settings.BaseURL), readiness(client, settings.BaseURL.JoinPath("health", "readiness")), nil
}
```

`catalogclient` decodes the catalog's JSON into a type of its own, and
imports nothing from `catalog/`, as a client of another team's service
couldn't:

```go
type Client struct {
	http *http.Client
	base *url.URL
}

func New(client *http.Client, baseURL *url.URL) *Client {
	return &Client{http: client, base: baseURL}
}

type product struct {
	UnitPrice *float64 `json:"unitPrice"`
}

const maxBody = 64 << 10
```

## The request

`Price` escapes the product's ID into the path:

```go
func (c *Client) Price(ctx context.Context, productID string) (float64, error) {
	u := c.base.JoinPath("products")
	u.RawPath = u.EscapedPath() + "/" + url.PathEscape(productID)
	u.Path += "/" + productID

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("getting product %s: %w", productID, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("getting product %s: %w: %w", productID, endpoint.ErrCatalogUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()
```

An error from `Do` is the catalog unavailable: it couldn't be reached
after the retries, the call's `Timeout` passed, or the breaker rejected the
call.

## The answers

Each status becomes a price, or an error the cart's operations answer:

```go
switch resp.StatusCode {
case http.StatusOK:
case http.StatusNotFound:
	return 0, fmt.Errorf("getting product %s: %w", productID, endpoint.ErrProductNotFound)
case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	return 0, fmt.Errorf("getting product %s: %w: the catalog answered %d", productID, endpoint.ErrCatalogUnavailable, resp.StatusCode)
default:
	return 0, fmt.Errorf("getting product %s: the catalog answered %d", productID, resp.StatusCode)
}

var p product
if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&p); err != nil {
	return 0, fmt.Errorf("getting product %s: decoding the catalog's answer: %w", productID, err)
}
switch {
case p.UnitPrice == nil:
	return 0, fmt.Errorf("getting product %s: the catalog answered with no unitPrice", productID)
case *p.UnitPrice < 0:
	return 0, fmt.Errorf("getting product %s: the catalog answered with a negative unitPrice", productID)
}
return *p.UnitPrice, nil
```

| The catalog | `Price` returns | The cart answers |
|---|---|---|
| answers 200 with a `unitPrice` of 0 or more | the price | 201, with the item |
| answers 404 | `ErrProductNotFound` | 422 |
| answers 502, 503 or 504, or can't be reached | `ErrCatalogUnavailable` | 503, the catalog can't be reached |
| is shed by the breaker | `ErrCatalogUnavailable`, wrapping `circuit.ErrRejected` | 503, the cart isn't calling it |
| answers anything else | an error | 500 |

The two 503s are `answer`'s:

```go
case errors.Is(err, ErrCatalogUnavailable) && errors.Is(err, circuit.ErrRejected):
	return unavailable("The cart isn't calling the catalog right now, since it has been failing. Try again later.")
case errors.Is(err, ErrCatalogUnavailable):
	return unavailable("The cart can't reach the catalog right now. Try again later.")
```

## Readiness

The cart's readiness asks the catalog's, through the same client:

```go
func readiness(client *http.Client, u *url.URL) func(context.Context) error {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
			_ = resp.Body.Close()
		}()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the catalog's readiness answered %d", resp.StatusCode)
		}
		return nil
	}
}
```

So it proves what adding an item needs: the URL, a catalog certificate the
CA signed, a client certificate the catalog trusts, and a catalog that has
reached its own database. [`Init`](/humus/walkthroughs/http/shoppingcart/init/#readiness)
latches it.

Next: [Init](/humus/walkthroughs/http/shoppingcart/init/)
