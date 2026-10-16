// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/z5labs/humus/humustest"
)

// chain returns the parsed leaf and CA certificates of c.
func chain(t *testing.T, c *humustest.Certificate) (leaf, ca *x509.Certificate) {
	t.Helper()
	tc := c.TLS()
	require.Len(t, tc.Certificate, 2)
	leaf, err := x509.ParseCertificate(tc.Certificate[0])
	require.NoError(t, err)
	ca, err = x509.ParseCertificate(tc.Certificate[1])
	require.NoError(t, err)
	return leaf, ca
}

func TestNewCA_generates_a_new_CA_each_call(t *testing.T) {
	_, one := chain(t, humustest.NewCA(t).Issue(t, "api.internal"))
	_, two := chain(t, humustest.NewCA(t).Issue(t, "api.internal"))

	require.True(t, one.IsCA)
	require.True(t, two.IsCA)
	require.NotEqual(t, one.SerialNumber, two.SerialNumber, "each CA has its own serial number")
	require.False(t, one.PublicKey.(*ecdsa.PublicKey).Equal(two.PublicKey), "each CA has its own key")
	require.LessOrEqual(t, one.SerialNumber.BitLen(), 128)
	require.Positive(t, one.SerialNumber.Sign())
}

func TestCA_Issue_generates_a_new_certificate_each_call(t *testing.T) {
	ca := humustest.NewCA(t)
	one, _ := chain(t, ca.Issue(t, "api.internal"))
	two, _ := chain(t, ca.Issue(t, "api.internal"))

	require.NotEqual(t, one.SerialNumber, two.SerialNumber, "each certificate has its own serial number")
	require.False(t, one.PublicKey.(*ecdsa.PublicKey).Equal(two.PublicKey), "each certificate has its own key")
}

func TestCA_Issue_puts_each_name_in_its_SAN_field(t *testing.T) {
	ca := humustest.NewCA(t)
	leaf, _ := chain(t, ca.Issue(t, "api.internal", "127.0.0.1", "*.svc.internal", "::1"))

	require.Equal(t, []string{"api.internal", "*.svc.internal"}, leaf.DNSNames)
	require.Len(t, leaf.IPAddresses, 2)
	require.True(t, leaf.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")))
	require.True(t, leaf.IPAddresses[1].Equal(net.ParseIP("::1")))

	for _, name := range []string{"api.internal", "a.svc.internal", "127.0.0.1", "::1"} {
		require.NoError(t, leaf.VerifyHostname(name), name)
	}
	require.Error(t, leaf.VerifyHostname("other.internal"))
}

func TestCA_Pool(t *testing.T) {
	ca := humustest.NewCA(t)
	other := humustest.NewCA(t)
	leaf, _ := chain(t, ca.Issue(t, "api.internal"))

	_, err := leaf.Verify(x509.VerifyOptions{DNSName: "api.internal", Roots: ca.Pool()})
	require.NoError(t, err, "a certificate verifies against its own CA's Pool")

	_, err = leaf.Verify(x509.VerifyOptions{DNSName: "api.internal", Roots: other.Pool()})
	_, ok := errors.AsType[x509.UnknownAuthorityError](err)
	require.True(t, ok, "a certificate fails verification against another CA's Pool: %v", err)
}

func TestCertificate_TLS_carries_the_leaf_then_the_CA(t *testing.T) {
	ca := humustest.NewCA(t)
	c := ca.Issue(t, "api.internal")
	tc := c.TLS()
	leaf, root := chain(t, c)

	require.False(t, leaf.IsCA)
	require.Equal(t, []string{"api.internal"}, leaf.DNSNames)
	require.True(t, root.IsCA)
	require.NoError(t, leaf.CheckSignatureFrom(root), "the second certificate is the CA that signed the first")
	require.Equal(t, leaf.Raw, tc.Leaf.Raw)

	key, ok := tc.PrivateKey.(*ecdsa.PrivateKey)
	require.True(t, ok)
	require.True(t, key.PublicKey.Equal(leaf.PublicKey), "the private key is the leaf's")
}

func TestCertificate_PKCS12(t *testing.T) {
	ca := humustest.NewCA(t)
	c := ca.Issue(t, "api.internal")
	tc := c.TLS()

	bundle, password := c.PKCS12(t)
	require.NotEmpty(t, password)

	key, leaf, caCerts, err := pkcs12.DecodeChain(bundle, password)
	require.NoError(t, err)
	require.True(t, tc.PrivateKey.(*ecdsa.PrivateKey).Equal(key), "the bundle holds the certificate's key")
	require.Equal(t, tc.Certificate[0], leaf.Raw, "the bundle holds the leaf")
	require.Len(t, caCerts, 1)
	require.Equal(t, tc.Certificate[1], caCerts[0].Raw, "the bundle holds the CA")

	_, _, _, err = pkcs12.DecodeChain(bundle, password+"x")
	require.ErrorIs(t, err, pkcs12.ErrIncorrectPassword)

	_, _, _, err = pkcs12.DecodeChain(bundle, "")
	require.ErrorIs(t, err, pkcs12.ErrIncorrectPassword)

	_, again := c.PKCS12(t)
	require.NotEqual(t, password, again, "each call makes a new password")
}

// TestNewCA_validity_follows_the_bubble_clock builds a CA inside a bubble,
// where time.Now() is the bubble's clock, not the wall clock.
func TestNewCA_validity_follows_the_bubble_clock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		ca := humustest.NewCA(t)
		leaf, root := chain(t, ca.Issue(t, "api.internal"))

		for _, cert := range []*x509.Certificate{leaf, root} {
			require.True(t, now.Add(-time.Hour).Equal(cert.NotBefore), "valid from an hour before: %v", cert.NotBefore)
			require.True(t, now.Add(24*time.Hour).Equal(cert.NotAfter), "valid until a day after: %v", cert.NotAfter)
		}

		_, err := leaf.Verify(x509.VerifyOptions{DNSName: "api.internal", Roots: ca.Pool()})
		require.NoError(t, err, "the certificate verifies on the bubble's clock")

		time.Sleep(25 * time.Hour)
		_, err = leaf.Verify(x509.VerifyOptions{DNSName: "api.internal", Roots: ca.Pool()})
		invalid, ok := errors.AsType[x509.CertificateInvalidError](err)
		require.True(t, ok, "%v", err)
		require.Equal(t, x509.Expired, invalid.Reason, "a day later on the bubble's clock, the certificate has expired")
	})
}

// fatalTB records the failure reported through Fatalf and stops the
// goroutine, as testing.T does.
type fatalTB struct {
	testing.TB
	mu    sync.Mutex
	fatal string
}

func (*fatalTB) Helper() {}

func (tb *fatalTB) Fatalf(format string, args ...any) {
	tb.mu.Lock()
	tb.fatal = fmt.Sprintf(format, args...)
	tb.mu.Unlock()
	runtime.Goexit()
}

func TestCA_Issue_fails_the_test_through_t(t *testing.T) {
	ca := humustest.NewCA(t)
	tb := &fatalTB{TB: t}

	var got *humustest.Certificate
	var wg sync.WaitGroup
	wg.Go(func() {
		got = ca.Issue(tb, "bücher.example")
	})
	wg.Wait()

	require.Nil(t, got)
	require.Contains(t, tb.fatal, "humustest: Issue")
	require.Contains(t, tb.fatal, "bücher.example", "the failure names what was asked for")
}

// serveTLS serves HTTPS with cert on ln, replying with the request's Host,
// until the test ends.
func serveTLS(t *testing.T, ln net.Listener, cert tls.Certificate) {
	t.Helper()
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, r.Host)
		}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		// A client that rejects the certificate makes the server log a
		// handshake error, which is expected.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	served := make(chan error, 1)
	go func() { served <- srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown(context.Background()))
		require.ErrorIs(t, <-served, http.ErrServerClosed)
	})
}

// getHTTPS sends a GET to url with client and returns the body.
func getHTTPS(t *testing.T, client *http.Client, url string) (string, error) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return string(b), nil
}

// requireHostnameError asserts that err is a failure to verify the
// certificate for the name dialled.
func requireHostnameError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	_, ok := errors.AsType[x509.HostnameError](err)
	require.True(t, ok, "the certificate doesn't cover the name: %v", err)
}

// TestCA_TLS_handshake_on_a_Host serves HTTPS on the in-memory network,
// inside a bubble.
func TestCA_TLS_handshake_on_a_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		serveTLS(t, listen(t, h, ":443"), ca.Issue(t, "api.internal").TLS())

		client := h.HTTPClient()
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}

		body, err := getHTTPS(t, client, "https://api.internal/")
		require.NoError(t, err)
		require.Equal(t, "api.internal", body)

		_, err = getHTTPS(t, client, "https://other.internal/")
		requireHostnameError(t, err)

		other := h.HTTPClient()
		other.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: humustest.NewCA(t).Pool()}
		_, err = getHTTPS(t, other, "https://api.internal/")
		_, ok := errors.AsType[x509.UnknownAuthorityError](err)
		require.True(t, ok, "a client trusting another CA rejects the certificate: %v", err)

		synctest.Wait()
	})
}
