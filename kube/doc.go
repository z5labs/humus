// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package kube reaches a Kubernetes API server with humus's lifecycle, with
// client-go, Kubernetes' own client, and no controller-runtime.
//
// It is in the nested module github.com/z5labs/humus/kube, so client-go and
// apimachinery stay out of the module graph of end users who don't talk to
// Kubernetes. The module is always built against the core module in the
// same commit.
//
// # The Client
//
// A [Client] reaches one API server through the App's Host, from a
// [Connection] the end user's Init fills. [NewClient] builds it, and opens
// no connection:
//
//	client, err := kube.NewClient(s, conn)
//	if err != nil {
//		return nil, err
//	}
//	kc, err := kubernetes.NewForConfig(client.Config())
//	if err != nil {
//		return nil, err
//	}
//
// [Client.Config] returns a rest.Config for client-go's own clients: the
// clientset kubernetes.NewForConfig returns, and the dynamic and metadata
// clients. Every one of them reaches the API server through the Client's
// transport, which:
//
//   - dials with s.Host.Dial;
//   - runs TLS with a clone of Connection.TLS, whose ServerName defaults to
//     Connection.Server's host;
//   - uses no proxy, whatever the environment says;
//   - over HTTP/2, sends a ping after 30s without a frame, and gives up on
//     the connection when 15s pass without the answer, so a watch on a dead
//     connection doesn't wait for TCP to give up;
//   - adds Connection.Token's bearer token to every request;
//   - traces and measures every request with otelhttp, on s.Telemetry's
//     providers and propagator, except a watch, which lasts as long as an
//     informer runs and has no span;
//   - doesn't limit the client's rate. client-go's default is 5 requests a
//     second, and the API server's priority and fairness limits each client
//     itself.
//
// # The Connection
//
// The end user fills the Connection. humus reads no KUBERNETES_SERVICE_HOST,
// no token file and no kubeconfig, so a test fills it with whatever its
// cluster needs, and nothing reads the real environment or disk behind the
// end user's back. The package reads no environment variable, reads no
// file, and defines no variable's name.
//
// In a pod, the kubelet sets KUBERNETES_SERVICE_HOST and
// KUBERNETES_SERVICE_PORT in every container, and mounts the service
// account's CA and token in /var/run/secrets/kubernetes.io/serviceaccount/.
// The end user's Init sources Server from the two variables, and reads the
// CA and the token through s.Host.FS:
//
//	const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount/"
//
//	ca, err := readFile(s.Host.FS, serviceAccount+"ca.crt") // the end user's helper
//	if err != nil {
//		return nil, err
//	}
//	roots := x509.NewCertPool()
//	roots.AppendCertsFromPEM(ca)
//
//	conn, err := config.Struct(func(r *config.Resolver) kube.Connection {
//		return kube.Connection{
//			// JoinHostPort brackets an IPv6 host.
//			Server: net.JoinHostPort(r.Get(env("KUBERNETES_SERVICE_HOST")), r.Get(env("KUBERNETES_SERVICE_PORT"))),
//			TLS:    &tls.Config{RootCAs: roots},
//			Token: func(context.Context) (string, error) {
//				b, err := readFile(s.Host.FS, serviceAccount+"token")
//				return string(bytes.TrimSpace(b)), err
//			},
//		}
//	}).Read(ctx)
//
// The Client calls Token at most once a minute, however many requests it
// sends, so a token the kubelet rotates is picked up within a minute. When a
// call fails, the Client keeps the last token, which a rotated service
// account token outlives, and the request goes on. Until a call has
// returned a token there is none to keep, so each request calls Token, and
// fails with its error. A nil Token sends no Authorization header, for a
// client that authenticates with a certificate in Connection.TLS.
//
// For local development, the end user fills the Connection from a
// kubeconfig's embedded data, as kind and k3s write it, with client-go's
// clientcmd.Load. The end user reads the file, through s.Host.FS:
//
//	b, err := readFile(s.Host.FS, path)
//	if err != nil {
//		return kube.Connection{}, err
//	}
//	kc, err := clientcmd.Load(b)
//	if err != nil {
//		return kube.Connection{}, err
//	}
//	current := kc.Contexts[kc.CurrentContext]
//	cluster, user := kc.Clusters[current.Cluster], kc.AuthInfos[current.AuthInfo]
//	u, err := url.Parse(cluster.Server)
//	if err != nil {
//		return kube.Connection{}, err
//	}
//	roots := x509.NewCertPool()
//	roots.AppendCertsFromPEM(cluster.CertificateAuthorityData)
//	cert, err := tls.X509KeyPair(user.ClientCertificateData, user.ClientKeyData)
//	if err != nil {
//		return kube.Connection{}, err
//	}
//	return kube.Connection{
//		Server: u.Host,
//		TLS:    &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{cert}},
//	}, nil
//
// humus has no helper for this. A kubeconfig's exec plugins and auth
// providers grow with every cloud, and a Connection is three fields.
// [CheckServer] and [CheckTLS] check the two required ones, and NewClient
// reports every bad field together.
//
// # What client-go still reaches
//
// client-go's feature gates read KUBE_FEATURE_* variables from the real
// environment, with os.LookupEnv, the first time a gate is used. The only
// way to replace them is features.ReplaceFeatureGates, which is global, so
// humus never calls it: this is an ambient leak humus can't close. Like the
// other ambient leaks the lifecycle documents, it isn't the end user's to
// pass in.
//
// The package sets no global of client-go's: no klog setting, no feature
// gate, and no metrics provider. Lint denies each of them in every humus
// module, and denies rest.InClusterConfig, which reads the environment and
// the disk itself. controller-runtime and controller-tools are denied too,
// since their init functions set globals that no wrapper can avoid.
package kube
