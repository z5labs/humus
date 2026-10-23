// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package certfile_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/shoppingcart/internal/certfile"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
)

const (
	certFile   = "tls/tls.crt"
	keyFile    = "tls/tls.key"
	serverName = "shoppingcart"
)

func encode(t *testing.T, c *humustest.Certificate) (certPEM, keyPEM []byte) {
	t.Helper()
	cert := c.TLS()
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}

func newHost(t *testing.T, c *humustest.Certificate) *humustest.Host {
	t.Helper()
	certPEM, keyPEM := encode(t, c)
	return humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		certFile: certPEM,
		keyFile:  keyPEM,
	}))
}

func write(t *testing.T, h *humustest.Host, name string, data []byte) {
	t.Helper()
	f, err := h.FS().OpenFile(h.Path(name), os.O_WRONLY|os.O_TRUNC, 0)
	require.NoError(t, err)
	_, err = f.Write(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func renew(t *testing.T, h *humustest.Host, c *humustest.Certificate) {
	t.Helper()
	certPEM, keyPEM := encode(t, c)
	write(t, h, certFile, certPEM)
	write(t, h, keyFile, keyPEM)
}

func config(t *testing.T, h *humustest.Host) *tls.Config {
	t.Helper()
	cfg, err := certfile.Config(h.FS(), h.Path(certFile), h.Path(keyFile))
	require.NoError(t, err)
	return cfg
}

func handshake(t *testing.T, server, client *tls.Config) (serverState, clientState tls.ConnectionState, err error) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	type result struct {
		state tls.ConnectionState
		err   error
	}
	served := make(chan result, 1)
	go func() {
		conn := tls.Server(serverConn, server)
		err := conn.Handshake()
		_ = serverConn.Close()
		served <- result{state: conn.ConnectionState(), err: err}
	}()

	conn := tls.Client(clientConn, client)
	clientErr := conn.Handshake()
	_ = clientConn.Close()
	s := <-served
	if s.err != nil {
		return tls.ConnectionState{}, tls.ConnectionState{}, s.err
	}
	if clientErr != nil {
		return tls.ConnectionState{}, tls.ConnectionState{}, clientErr
	}
	return s.state, conn.ConnectionState(), nil
}

func serverCertificate(t *testing.T, server *tls.Config, ca *humustest.CA) (*x509.Certificate, error) {
	t.Helper()
	_, state, err := handshake(t, server, &tls.Config{RootCAs: ca.Pool(), ServerName: serverName})
	if err != nil {
		return nil, err
	}
	return state.PeerCertificates[0], nil
}

func clientCertificate(t *testing.T, client *tls.Config, ca *humustest.CA) (*x509.Certificate, error) {
	t.Helper()
	client = client.Clone()
	client.RootCAs = ca.Pool()
	client.ServerName = serverName
	server := &tls.Config{
		Certificates: []tls.Certificate{ca.Issue(t, serverName).TLS()},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool(),
	}
	state, _, err := handshake(t, server, client)
	if err != nil {
		return nil, err
	}
	return state.PeerCertificates[0], nil
}

func TestConfig(t *testing.T) {
	roles := map[string]func(*testing.T, *tls.Config, *humustest.CA) (*x509.Certificate, error){
		"server": serverCertificate,
		"client": clientCertificate,
	}
	for role, peerCertificate := range roles {
		t.Run(role, func(t *testing.T) {
			t.Run("a handshake gets the certificate from the files", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ca := humustest.NewCA(t)
					cert := ca.Issue(t, serverName)
					h := newHost(t, cert)

					got, err := peerCertificate(t, config(t, h), ca)
					require.NoError(t, err)
					require.True(t, got.Equal(cert.TLS().Leaf))
				})
			})

			t.Run("after the files are rewritten, a handshake 30s later gets the new certificate", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ca := humustest.NewCA(t)
					old := ca.Issue(t, serverName)
					h := newHost(t, old)
					cfg := config(t, h)

					renewed := ca.Issue(t, serverName)
					renew(t, h, renewed)

					time.Sleep(30*time.Second - time.Nanosecond)
					got, err := peerCertificate(t, cfg, ca)
					require.NoError(t, err)
					require.True(t, got.Equal(old.TLS().Leaf))

					time.Sleep(time.Nanosecond)
					got, err = peerCertificate(t, cfg, ca)
					require.NoError(t, err)
					require.True(t, got.Equal(renewed.TLS().Leaf))
				})
			})

			t.Run("a later read that fails keeps the last pair", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ca := humustest.NewCA(t)
					old := ca.Issue(t, serverName)
					h := newHost(t, old)
					cfg := config(t, h)

					mismatched, _ := encode(t, ca.Issue(t, serverName))
					write(t, h, certFile, mismatched)
					time.Sleep(30 * time.Second)
					got, err := peerCertificate(t, cfg, ca)
					require.NoError(t, err)
					require.True(t, got.Equal(old.TLS().Leaf))

					require.NoError(t, h.FS().Remove(h.Path(keyFile)))
					time.Sleep(30 * time.Second)
					got, err = peerCertificate(t, cfg, ca)
					require.NoError(t, err)
					require.True(t, got.Equal(old.TLS().Leaf))

					renewed := ca.Issue(t, serverName)
					certPEM, keyPEM := encode(t, renewed)
					write(t, h, certFile, certPEM)
					f, err := h.FS().OpenFile(h.Path(keyFile), os.O_WRONLY|os.O_CREATE, 0o600)
					require.NoError(t, err)
					_, err = f.Write(keyPEM)
					require.NoError(t, err)
					require.NoError(t, f.Close())
					time.Sleep(30 * time.Second)
					got, err = peerCertificate(t, cfg, ca)
					require.NoError(t, err)
					require.True(t, got.Equal(renewed.TLS().Leaf))
				})
			})
		})
	}

	t.Run("a pair that does not match fails the first read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			certPEM, _ := encode(t, ca.Issue(t, serverName))
			_, keyPEM := encode(t, ca.Issue(t, serverName))
			h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{certFile: certPEM, keyFile: keyPEM}))

			_, err := certfile.Config(h.FS(), h.Path(certFile), h.Path(keyFile))
			require.ErrorContains(t, err, h.Path(certFile))
		})
	})

	t.Run("a file that cannot be read fails the first read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			certPEM, _ := encode(t, ca.Issue(t, serverName))
			h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{certFile: certPEM}))

			_, err := certfile.Config(h.FS(), h.Path(certFile), h.Path(keyFile))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	})

	t.Run("httpserver.CheckTLS and httpclient.CheckTLS accept the config", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			cfg := config(t, newHost(t, ca.Issue(t, serverName)))
			require.NoError(t, httpserver.CheckTLS(cfg))

			client := cfg.Clone()
			client.RootCAs = ca.Pool()
			require.NoError(t, httpclient.CheckTLS(client))
		})
	})
}
