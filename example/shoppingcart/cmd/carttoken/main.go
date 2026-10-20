// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command carttoken makes the shoppingcart's key set and signs its tokens by hand.
package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

const (
	jwksCommand = "jwks"
	signCommand = "sign"
)

func main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}

func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "carttoken"
	return telemetry.New(ctx, s, dial, p.Stderr)
}

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}
	return []humus.Service{job.NewService(s, func(context.Context) error {
		var out string
		var err error
		switch settings.Command {
		case jwksCommand:
			out, err = keySet(settings.Key)
		case signCommand:
			out, err = sign(settings, time.Now())
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(s.Process.Stdout, out)
		return err
	})}, nil
}

func keySet(key jose.JSONWebKey) (string, error) {
	b, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{key.Public()}})
	return string(b), err
}

func sign(s settings, now time.Time) (string, error) {
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.SignatureAlgorithm(s.Key.Algorithm), Key: s.Key},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", err
	}
	claims := struct {
		josejwt.Claims
		Scope string `json:"scope,omitempty"`
	}{
		Claims: josejwt.Claims{
			Issuer:    s.Issuer,
			Audience:  josejwt.Audience{s.Audience},
			Subject:   s.Subject,
			IssuedAt:  josejwt.NewNumericDate(now),
			NotBefore: josejwt.NewNumericDate(now),
			Expiry:    josejwt.NewNumericDate(now.Add(s.TTL)),
		},
		Scope: strings.Join(s.Scopes, " "),
	}
	return josejwt.Signed(signer).Claims(claims).Serialize()
}

type settings struct {
	Command  string
	Key      jose.JSONWebKey
	Subject  string
	Scopes   []string
	Issuer   string
	Audience string
	TTL      time.Duration
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	args, err := parseArgs(s.Process)
	if err != nil {
		return settings{}, err
	}
	return config.Struct(func(r *config.Resolver) settings {
		set := settings{
			Command: args.command,
			Key:     r.Get(args.value("key").Parse(readFile(s.Host.FS)).Parse(parseKey)),
		}
		if args.command != signCommand {
			return set
		}
		set.Subject = r.Get(args.value("sub").Validate(notEmpty))
		set.Scopes = r.Get(args.scopesValue().Default(nil).Validate(checkScopes))
		set.Issuer = r.Get(args.value("iss").Validate(jwt.CheckIssuer))
		set.Audience = r.Get(args.value("aud").Validate(jwt.CheckAudience))
		set.TTL = r.Get(args.value("ttl").Parse(time.ParseDuration).Default(time.Hour).Validate(positive))
		return set
	}).Read(ctx)
}

type arguments struct {
	command string
	flags   map[string]string
	scopes  []string
}

func parseArgs(p bedrock.Process) (arguments, error) {
	name := p.Args[0]
	usage := func() {
		_, _ = fmt.Fprintf(p.Stderr, "Usage:\n  %s %s -key FILE\n  %s %s -key FILE -sub SUBJECT [-scope SCOPE]... -iss ISSUER -aud AUDIENCE [-ttl DURATION]\n",
			name, jwksCommand, name, signCommand)
	}
	if len(p.Args) < 2 {
		usage()
		return arguments{}, &config.Error{Name: "command", Err: config.ErrNotFound}
	}

	a := arguments{command: p.Args[1], flags: map[string]string{}}
	fs := flag.NewFlagSet(name+" "+a.command, flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	str := func(flagName, usage string) {
		fs.Func(flagName, usage, func(v string) error {
			a.flags[flagName] = v
			return nil
		})
	}
	str("key", "the PEM file holding the private key")
	switch a.command {
	case jwksCommand:
	case signCommand:
		str("sub", "the token's subject")
		fs.Func("scope", "a scope the token grants, repeated for each one", func(v string) error {
			a.scopes = append(a.scopes, v)
			return nil
		})
		str("iss", "the token's issuer")
		str("aud", "the token's audience")
		str("ttl", "how long the token is valid for (default 1h)")
	default:
		usage()
		return arguments{}, &config.Error{Name: "command", Err: fmt.Errorf("%q is neither %s nor %s", a.command, jwksCommand, signCommand)}
	}
	if err := fs.Parse(p.Args[2:]); err != nil {
		return arguments{}, &config.Error{Name: "arguments", Err: err}
	}
	if fs.NArg() > 0 {
		return arguments{}, &config.Error{Name: "arguments", Err: fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	return a, nil
}

func (a arguments) value(flagName string) config.Value[string] {
	return config.Func("-"+flagName, func(context.Context) (string, error) {
		v, ok := a.flags[flagName]
		if !ok {
			return "", config.ErrNotFound
		}
		return v, nil
	})
}

func (a arguments) scopesValue() config.Value[[]string] {
	return config.Func("-scope", func(context.Context) ([]string, error) {
		if len(a.scopes) == 0 {
			return nil, config.ErrNotFound
		}
		return a.scopes, nil
	})
}

func parseKey(b []byte) (jose.JSONWebKey, error) {
	var key crypto.Signer
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		var parsed any
		var err error
		switch block.Type {
		case "PRIVATE KEY":
			parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			parsed, err = x509.ParseECPrivateKey(block.Bytes)
		case "RSA PRIVATE KEY":
			parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		default:
			continue
		}
		if err != nil {
			return jose.JSONWebKey{}, err
		}
		if key != nil {
			return jose.JSONWebKey{}, errors.New("more than one private key in the file")
		}
		signer, ok := parsed.(crypto.Signer)
		if !ok {
			return jose.JSONWebKey{}, fmt.Errorf("a %T can't sign", parsed)
		}
		key = signer
	}
	if key == nil {
		return jose.JSONWebKey{}, errors.New("no PEM private key in the file")
	}

	alg, err := algorithm(key)
	if err != nil {
		return jose.JSONWebKey{}, err
	}
	jwk := jose.JSONWebKey{Key: key, Algorithm: string(alg), Use: "sig"}
	thumbprint, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return jose.JSONWebKey{}, err
	}
	jwk.KeyID = base64.RawURLEncoding.EncodeToString(thumbprint)
	return jwk, nil
}

func algorithm(key crypto.Signer) (jwt.Algorithm, error) {
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		switch k.Curve {
		case elliptic.P256():
			return jwt.ES256, nil
		case elliptic.P384():
			return jwt.ES384, nil
		case elliptic.P521():
			return jwt.ES512, nil
		}
		return "", fmt.Errorf("the curve %s isn't P-256, P-384 or P-521", k.Curve.Params().Name)
	case *rsa.PrivateKey:
		if k.N.BitLen() < 2048 {
			return "", fmt.Errorf("the RSA key has %d bits, fewer than 2048", k.N.BitLen())
		}
		return jwt.RS256, nil
	case ed25519.PrivateKey:
		return jwt.EdDSA, nil
	}
	return "", fmt.Errorf("a %T isn't an RSA, ECDSA or Ed25519 key", key)
}

func notEmpty(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func checkScopes(scopes []string) error {
	for _, scope := range scopes {
		if scope == "" || strings.ContainsFunc(scope, unicode.IsSpace) {
			return fmt.Errorf("%q is empty or has a space", scope)
		}
	}
	return nil
}

func positive(d time.Duration) error {
	if d <= 0 {
		return errors.New("must be positive")
	}
	return nil
}

func readFile(fsys humus.FS) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}
