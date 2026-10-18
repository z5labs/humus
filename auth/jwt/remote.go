// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package jwt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	jose "github.com/go-jose/go-jose/v4"
)

// maxKeySetBytes is the most of a key set's body Keys reads.
const maxKeySetBytes = 1 << 20

// RemoteKeySet is the KeySet of a JSON Web Key Set, as RFC 7517 has it,
// served at an https URL: an identity provider's jwks_uri.
//
// It keeps no keys and records nothing of its own: the Verifier holds the
// keys and decides when Keys is called, and each fetch is traced and
// measured by the client's own telemetry, such as an httpclient client's.
type RemoteKeySet struct {
	url    string
	client *http.Client
}

// NewRemoteKeySet returns the RemoteKeySet of u, which it fetches with
// client: the end user's httpclient client for u's origin. It returns one
// error that names every mistake: a nil client, and a u that isn't an
// absolute https URL with a host.
//
// The URL must be https: a key set fetched in plain text would let anyone
// on the path add a key and sign tokens the service accepts.
func NewRemoteKeySet(u *url.URL, client *http.Client) (*RemoteKeySet, error) {
	var errs []error
	if client == nil {
		errs = append(errs, errors.New("the client must not be nil"))
	}
	switch {
	case u == nil:
		errs = append(errs, errors.New("the URL must not be nil"))
	case !u.IsAbs():
		errs = append(errs, fmt.Errorf("the URL %q isn't absolute", u.String()))
	default:
		if u.Scheme != "https" {
			errs = append(errs, fmt.Errorf("the URL %q isn't https: a key set fetched in plain text lets anyone on the path add a key", u.String()))
		}
		if u.Host == "" {
			errs = append(errs, fmt.Errorf("the URL %q has no host", u.String()))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("jwt: NewRemoteKeySet: %w", err)
	}
	return &RemoteKeySet{url: u.String(), client: client}, nil
}

// Keys fetches the set with the client, and returns its signing keys. It
// reads at most 1 MiB of the body, and fails on a larger one, on a status
// other than 200, and on a body that isn't a JSON Web Key Set, with an
// error that names the URL. It skips a key without kid, with a use other
// than sig, with private parts, or of a type go-jose doesn't read, rather
// than failing the set.
//
// The client bounds the fetch, such as with httpclient's Timeout.
func (k *RemoteKeySet) Keys(ctx context.Context) ([]Key, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, k.fail(err)
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, k.fail(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, k.fail(fmt.Errorf("the status is %s, not 200 OK", resp.Status))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeySetBytes+1))
	if err != nil {
		return nil, k.fail(fmt.Errorf("reading the body: %w", err))
	}
	if len(body) > maxKeySetBytes {
		return nil, k.fail(errors.New("the body is over 1 MiB"))
	}
	keys, err := parseKeySet(body)
	if err != nil {
		return nil, k.fail(err)
	}
	return keys, nil
}

// fail returns err, after the URL of the set whose fetch it failed.
func (k *RemoteKeySet) fail(err error) error {
	return fmt.Errorf("jwt: fetching the JSON Web Key Set at %s: %w", k.url, err)
}

// parseKeySet returns the signing keys of a JSON Web Key Set. It fails on
// a body that isn't a JSON object with a keys array. Each key is read on
// its own, so one go-jose doesn't read is skipped rather than failing the
// set.
func parseKeySet(body []byte) ([]Key, error) {
	var set struct {
		Keys *[]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("the body isn't a JSON Web Key Set: %w", err)
	}
	if set.Keys == nil {
		return nil, errors.New("the body isn't a JSON Web Key Set: it has no keys array")
	}
	keys := make([]Key, 0, len(*set.Keys))
	for _, raw := range *set.Keys {
		var jwk jose.JSONWebKey
		if err := jwk.UnmarshalJSON(raw); err != nil {
			continue // a type, or a curve, go-jose doesn't read
		}
		if jwk.KeyID == "" || (jwk.Use != "" && jwk.Use != "sig") || !jwk.IsPublic() {
			continue
		}
		keys = append(keys, Key{ID: jwk.KeyID, Public: jwk.Key})
	}
	return keys, nil
}
