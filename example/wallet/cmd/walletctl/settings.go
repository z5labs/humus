// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/example/wallet/server"
	"github.com/z5labs/humus/grpc/grpcclient"
)

const (
	jwksCommand    = "jwks"
	tokenCommand   = "token"
	openCommand    = "open"
	balanceCommand = "balance"
	sendCommand    = "send"
	watchCommand   = "watch"
	depositCommand = "deposit"
)

const callTokenTTL = 5 * time.Minute

type flagSpec struct {
	name     string
	arg      string
	usage    string
	optional bool
	repeated bool
}

var (
	keyFlag        = flagSpec{name: "key", arg: "FILE", usage: "the PEM file holding the signing key"}
	subFlag        = flagSpec{name: "sub", arg: "SUBJECT", usage: "the token's subject"}
	scopeFlag      = flagSpec{name: "scope", arg: "SCOPE", usage: "a scope the token grants, repeated for each one", optional: true, repeated: true}
	issFlag        = flagSpec{name: "iss", arg: "ISSUER", usage: "the token's issuer"}
	audFlag        = flagSpec{name: "aud", arg: "AUDIENCE", usage: "the token's audience"}
	ttlFlag        = flagSpec{name: "ttl", arg: "DURATION", usage: "how long the token is valid for (default 1h)", optional: true}
	addrFlag       = flagSpec{name: "addr", arg: "HOST:PORT", usage: "the address to call"}
	caFlag         = flagSpec{name: "ca", arg: "FILE", usage: "the PEM file of the CA certificates the server is trusted through"}
	serverNameFlag = flagSpec{name: "server-name", arg: "NAME", usage: "the name the server's certificate is verified for (default the host of -addr)", optional: true}
	certFlag       = flagSpec{name: "cert", arg: "FILE", usage: "the PEM file holding the client certificate and its private key"}
	toFlag         = flagSpec{name: "to", arg: "ACCOUNT", usage: "the account to send to"}
	accountFlag    = flagSpec{name: "account", arg: "ACCOUNT", usage: "the account to deposit to"}
	amountFlag     = flagSpec{name: "amount", arg: "CENTS", usage: "the amount, in cents"}
	idFlag         = flagSpec{name: "id", arg: "ID", usage: "the transfer's ID, sent again to repeat it (default a random one)", optional: true}
)

var walletFlags = []flagSpec{addrFlag, caFlag, serverNameFlag, keyFlag, subFlag, issFlag, audFlag}

type command struct {
	name  string
	flags []flagSpec
}

var commands = []command{
	{jwksCommand, []flagSpec{keyFlag}},
	{tokenCommand, []flagSpec{keyFlag, subFlag, scopeFlag, issFlag, audFlag, ttlFlag}},
	{openCommand, walletFlags},
	{balanceCommand, walletFlags},
	{sendCommand, append(slices.Clone(walletFlags), toFlag, amountFlag, idFlag)},
	{watchCommand, walletFlags},
	{depositCommand, []flagSpec{addrFlag, caFlag, serverNameFlag, certFlag, accountFlag, amountFlag, idFlag}},
}

var scopeFor = map[string]string{
	openCommand:    server.ScopeWrite,
	balanceCommand: server.ScopeRead,
	sendCommand:    server.ScopeWrite,
	watchCommand:   server.ScopeRead,
}

type settings struct {
	Command     string
	Key         jose.JSONWebKey
	Token       tokenSettings
	Client      grpcclient.Settings
	To          string
	Account     string
	AmountCents int64
	ID          string
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	args, err := parseArgs(s.Process)
	if err != nil {
		return settings{}, err
	}
	file := func(name string) config.Value[[]byte] { return args.value(name).Parse(readFile(s.Host.FS)) }

	return config.Struct(func(r *config.Resolver) settings {
		set := settings{Command: args.command}
		if args.accepts(keyFlag) {
			set.Key = r.Get(file("key").Parse(parseKey))
		}
		switch args.command {
		case jwksCommand:
			return set
		case tokenCommand:
			set.Token = tokenSettings{
				Subject:  r.Get(args.value("sub").Validate(notEmpty)),
				Scopes:   r.Get(args.scopesValue().Default(nil).Validate(checkScopes)),
				Issuer:   r.Get(args.value("iss").Validate(jwt.CheckIssuer)),
				Audience: r.Get(args.value("aud").Validate(jwt.CheckAudience)),
				TTL:      r.Get(args.value("ttl").Parse(time.ParseDuration).Default(time.Hour).Validate(positive)),
			}
			return set
		case depositCommand:
			set.Account = r.Get(args.value("account").Validate(notEmpty))
		case sendCommand:
			set.To = r.Get(args.value("to").Validate(notEmpty))
		}
		if args.accepts(subFlag) {
			set.Token = tokenSettings{
				Subject:  r.Get(args.value("sub").Validate(notEmpty)),
				Scopes:   []string{scopeFor[args.command]},
				Issuer:   r.Get(args.value("iss").Validate(jwt.CheckIssuer)),
				Audience: r.Get(args.value("aud").Validate(jwt.CheckAudience)),
				TTL:      callTokenTTL,
			}
		}
		if args.accepts(amountFlag) {
			set.AmountCents = r.Get(args.value("amount").Parse(parseCents).Validate(positiveCents))
			set.ID = r.Get(args.value("id").Default(rand.Text()).Validate(notEmpty))
		}

		client := grpcclient.DefaultSettings()
		client.Addr = r.Get(args.value("addr").Validate(grpcclient.CheckAddr))
		client.TLS = &tls.Config{
			RootCAs:    r.Get(file("ca").Parse(certPool)),
			ServerName: r.Get(args.value("server-name").Default("")),
			MinVersion: tls.VersionTLS12,
		}
		if args.accepts(certFlag) {
			client.TLS.Certificates = []tls.Certificate{r.Get(file("cert").Parse(keyPair))}
		}
		set.Client = client
		return set
	}).Read(ctx)
}

type arguments struct {
	command string
	flags   []flagSpec
	values  map[string]string
	scopes  []string
}

func parseArgs(p bedrock.Process) (arguments, error) {
	name := p.Args[0]
	usage := func() {
		var b strings.Builder
		b.WriteString("Usage:\n")
		for _, c := range commands {
			fmt.Fprintf(&b, "  %s %s", name, c.name)
			for _, f := range c.flags {
				arg := "-" + f.name + " " + f.arg
				switch {
				case f.repeated:
					arg = "[" + arg + "]..."
				case f.optional:
					arg = "[" + arg + "]"
				}
				b.WriteString(" " + arg)
			}
			b.WriteString("\n")
		}
		_, _ = io.WriteString(p.Stderr, b.String())
	}
	if len(p.Args) < 2 {
		usage()
		return arguments{}, &config.Error{Name: "command", Err: config.ErrNotFound}
	}

	a := arguments{command: p.Args[1], values: map[string]string{}}
	i := slices.IndexFunc(commands, func(c command) bool { return c.name == a.command })
	if i < 0 {
		usage()
		names := make([]string, len(commands))
		for j, c := range commands {
			names[j] = c.name
		}
		return arguments{}, &config.Error{Name: "command", Err: fmt.Errorf("%q isn't one of %s or %s", a.command, strings.Join(names[:len(names)-1], ", "), names[len(names)-1])}
	}
	a.flags = commands[i].flags

	fs := flag.NewFlagSet(name+" "+a.command, flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	for _, f := range a.flags {
		if f.repeated {
			fs.Func(f.name, f.usage, func(v string) error {
				a.scopes = append(a.scopes, v)
				return nil
			})
			continue
		}
		fs.Func(f.name, f.usage, func(v string) error {
			a.values[f.name] = v
			return nil
		})
	}
	if err := fs.Parse(p.Args[2:]); err != nil {
		return arguments{}, &config.Error{Name: "arguments", Err: err}
	}
	if fs.NArg() > 0 {
		return arguments{}, &config.Error{Name: "arguments", Err: fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	return a, nil
}

func (a arguments) accepts(f flagSpec) bool {
	return slices.ContainsFunc(a.flags, func(g flagSpec) bool { return g.name == f.name })
}

func (a arguments) value(flagName string) config.Value[string] {
	return config.Func("-"+flagName, func(context.Context) (string, error) {
		v, ok := a.values[flagName]
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

func certPool(b []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(b) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}

func keyPair(b []byte) (tls.Certificate, error) {
	return tls.X509KeyPair(b, b)
}

func parseCents(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errors.New("must be a whole number of cents")
	}
	return n, nil
}

func positiveCents(n int64) error {
	if n <= 0 {
		return errors.New("must be positive")
	}
	return nil
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
			return fmt.Errorf("%q is empty or has whitespace", scope)
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
