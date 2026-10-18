// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/z5labs/humus/grpc/grpcserver"
)

// A Service behind a mesh trusts the failure scenario of a caller whose
// client certificate the mesh's CA verified. The failure injection page of
// the docs site quotes this example.
func ExampleSettings_trustScenario() {
	meshCAs := x509.NewCertPool() // the mesh's CA certificates
	settings := grpcserver.Settings{
		Addr: ":9443",
		TLS:  &tls.Config{}, // the Service's certificate, sourced as usual
	}

	settings.TLS.ClientAuth = tls.VerifyClientCertIfGiven
	settings.TLS.ClientCAs = meshCAs
	settings.TrustScenario = func(ctx context.Context, _ string) bool {
		p, ok := peer.FromContext(ctx)
		if !ok {
			return false
		}
		info, ok := p.AuthInfo.(credentials.TLSInfo)
		return ok && len(info.State.VerifiedChains) > 0
	}

	verified := credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{}}}}
	inMesh := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: verified})
	outside := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{}})
	fmt.Println(settings.TrustScenario(inMesh, "/payments.v1.Payments/Charge"))
	fmt.Println(settings.TrustScenario(outside, "/payments.v1.Payments/Charge"))
	// Output:
	// true
	// false
}
