// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package certfile_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus/example/wallet/internal/certfile"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
)

const (
	pemFile    = "tls/tls.pem"
	serverName = "ledger"
	addr       = "ledger:9090"
)

func encode(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	return append(certificates(t, c), key(t, c)...)
}

func newHost(t *testing.T, c *humustest.Certificate) *humustest.Host {
	t.Helper()
	return humustest.NewHost(t, humustest.WithFiles(map[string][]byte{pemFile: encode(t, c)}))
}

func config(t *testing.T, h *humustest.Host) *tls.Config {
	t.Helper()
	cfg, err := certfile.Config(h.FS(), h.Path(pemFile))
	require.NoError(t, err)
	return cfg
}

func writeInPlace(t *testing.T, h *humustest.Host, data []byte) {
	t.Helper()
	f, err := h.FS().OpenFile(h.Path(pemFile), os.O_WRONLY|os.O_TRUNC, 0)
	require.NoError(t, err)
	_, err = f.Write(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func writeByRename(t *testing.T, h *humustest.Host, data []byte) {
	t.Helper()
	name := h.Path(pemFile)
	tmp, err := h.FS().CreateTemp(filepath.Dir(name), "renewed-*")
	require.NoError(t, err)
	_, err = tmp.Write(data)
	require.NoError(t, err)
	require.NoError(t, tmp.Close())
	require.NoError(t, h.FS().Rename(tmp.Name(), name))
}

type server struct {
	presented atomic.Pointer[x509.Certificate]
}

func serve(t *testing.T, h *humustest.Host, cfg *tls.Config) *server {
	t.Helper()
	l, err := h.Listen("tcp", addr)
	require.NoError(t, err)

	s := &server{}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(cfg)),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if p, ok := peer.FromContext(ctx); ok {
				if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
					s.presented.Store(info.State.PeerCertificates[0])
				}
			}
			return handler(ctx, req)
		}),
	)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		srv.Stop()
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("Serve: %v", err)
		}
	})
	return s
}

func check(t *testing.T, conn *grpc.ClientConn, opts ...grpc.CallOption) error {
	t.Helper()
	_, err := healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{}, opts...)
	return err
}

type role struct {
	start func(t *testing.T, h *humustest.Host, ca *humustest.CA) func(*testing.T) (*x509.Certificate, error)
}

var roles = map[string]role{
	"the server presents": {
		start: func(t *testing.T, h *humustest.Host, ca *humustest.CA) func(*testing.T) (*x509.Certificate, error) {
			serve(t, h, config(t, h))
			return func(t *testing.T) (*x509.Certificate, error) {
				var p peer.Peer
				if err := check(t, grpcservertest.NewClient(t, h, addr, ca.Pool()), grpc.Peer(&p)); err != nil {
					return nil, err
				}
				return p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0], nil
			}
		},
	},
	"the client presents": {
		start: func(t *testing.T, h *humustest.Host, ca *humustest.CA) func(*testing.T) (*x509.Certificate, error) {
			s := serve(t, h, &tls.Config{
				Certificates: []tls.Certificate{ca.Issue(t, serverName).TLS()},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    ca.Pool(),
			})
			client := config(t, h)
			client.RootCAs = ca.Pool()
			client.ServerName = serverName
			return func(t *testing.T) (*x509.Certificate, error) {
				s.presented.Store(nil)
				conn := grpcservertest.NewClient(t, h, addr, ca.Pool(), grpc.WithTransportCredentials(credentials.NewTLS(client)))
				if err := check(t, conn); err != nil {
					return nil, err
				}
				return s.presented.Load(), nil
			}
		},
	},
}

var rewrites = map[string]func(*testing.T, *humustest.Host, []byte){
	"in place":  writeInPlace,
	"by rename": writeByRename,
}

func TestConfig(t *testing.T) {
	for name, r := range roles {
		t.Run(name, func(t *testing.T) {
			t.Run("the certificate from the file", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ca := humustest.NewCA(t)
					cert := ca.Issue(t, serverName)
					h := newHost(t, cert)
					presented := r.start(t, h, ca)

					got, err := presented(t)
					require.NoError(t, err)
					require.NotNil(t, got)
					require.True(t, got.Equal(cert.TLS().Leaf))
				})
			})

			for how, rewrite := range rewrites {
				t.Run("the renewed certificate after the file is rewritten "+how, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						ca := humustest.NewCA(t)
						old := ca.Issue(t, serverName)
						h := newHost(t, old)
						presented := r.start(t, h, ca)

						got, err := presented(t)
						require.NoError(t, err)
						require.NotNil(t, got)
						require.True(t, got.Equal(old.TLS().Leaf))

						renewed := ca.Issue(t, serverName)
						rewrite(t, h, encode(t, renewed))

						got, err = presented(t)
						require.NoError(t, err)
						require.NotNil(t, got)
						require.True(t, got.Equal(renewed.TLS().Leaf))
						require.False(t, got.Equal(old.TLS().Leaf))
					})
				})
			}

			t.Run("no certificate while the file holds a certificate and a key that do not match", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ca := humustest.NewCA(t)
					h := newHost(t, ca.Issue(t, serverName))
					presented := r.start(t, h, ca)

					mismatched := append(certificates(t, ca.Issue(t, serverName)), key(t, ca.Issue(t, serverName))...)
					writeByRename(t, h, mismatched)

					_, err := presented(t)
					require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)
				})
			})
		})
	}

	t.Run("it fails when the file is missing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			_, err := certfile.Config(h.FS(), h.Path(pemFile))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	})

	t.Run("it fails when the file holds no private key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{pemFile: certificates(t, ca.Issue(t, serverName))}))

			_, err := certfile.Config(h.FS(), h.Path(pemFile))
			require.ErrorContains(t, err, h.Path(pemFile))
		})
	})

	t.Run("it fails when the certificate and the key do not match", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			mismatched := append(certificates(t, ca.Issue(t, serverName)), key(t, ca.Issue(t, serverName))...)
			h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{pemFile: mismatched}))

			_, err := certfile.Config(h.FS(), h.Path(pemFile))
			require.ErrorContains(t, err, h.Path(pemFile))
		})
	})

	t.Run("grpcserver.CheckTLS and grpcclient.CheckTLS accept the config", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			cfg := config(t, newHost(t, ca.Issue(t, serverName)))
			require.NoError(t, grpcserver.CheckTLS(cfg))

			client := cfg.Clone()
			client.RootCAs = ca.Pool()
			require.NoError(t, grpcclient.CheckTLS(client))
		})
	})
}

func certificates(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	var data []byte
	for _, der := range c.TLS().Certificate {
		data = append(data, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return data
}

func key(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(c.TLS().PrivateKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
