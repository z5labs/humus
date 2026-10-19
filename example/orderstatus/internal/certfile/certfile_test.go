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

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/orderstatus/internal/certfile"
	"github.com/z5labs/humus/humustest"
)

const (
	certFile   = "tls/tls.crt"
	keyFile    = "tls/tls.key"
	serverName = "orderstatus"
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
	server := &tls.Config{
		Certificates: []tls.Certificate{ca.Issue(t, serverName).TLS()},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.Pool(),
	}
	client = client.Clone()
	client.RootCAs = ca.Pool()
	client.ServerName = serverName
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
				ca := humustest.NewCA(t)
				cert := ca.Issue(t, serverName)
				h := newHost(t, cert)

				got, err := peerCertificate(t, config(t, h), ca)
				require.NoError(t, err)
				require.True(t, got.Equal(cert.TLS().Leaf))
			})

			t.Run("after the files change, the next handshake gets the new certificate", func(t *testing.T) {
				ca := humustest.NewCA(t)
				old := ca.Issue(t, serverName)
				h := newHost(t, old)
				cfg := config(t, h)

				got, err := peerCertificate(t, cfg, ca)
				require.NoError(t, err)
				require.True(t, got.Equal(old.TLS().Leaf))

				renewed := ca.Issue(t, serverName)
				renew(t, h, renewed)

				got, err = peerCertificate(t, cfg, ca)
				require.NoError(t, err)
				require.True(t, got.Equal(renewed.TLS().Leaf))
				require.False(t, got.Equal(old.TLS().Leaf))
			})

			t.Run("a handshake fails while the files hold a certificate and a key that do not match", func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := newHost(t, ca.Issue(t, serverName))
				cfg := config(t, h)

				certPEM, _ := encode(t, ca.Issue(t, serverName))
				write(t, h, certFile, certPEM)

				_, err := peerCertificate(t, cfg, ca)
				require.Error(t, err)
			})
		})
	}

	t.Run("it fails when a file is missing", func(t *testing.T) {
		ca := humustest.NewCA(t)
		certPEM, _ := encode(t, ca.Issue(t, serverName))
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{certFile: certPEM}))

		_, err := certfile.Config(h.FS(), h.Path(certFile), h.Path(keyFile))
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("it fails when the certificate and the key do not match", func(t *testing.T) {
		ca := humustest.NewCA(t)
		certPEM, _ := encode(t, ca.Issue(t, serverName))
		_, keyPEM := encode(t, ca.Issue(t, serverName))
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{certFile: certPEM, keyFile: keyPEM}))

		_, err := certfile.Config(h.FS(), h.Path(certFile), h.Path(keyFile))
		require.ErrorContains(t, err, h.Path(certFile))
	})
}
