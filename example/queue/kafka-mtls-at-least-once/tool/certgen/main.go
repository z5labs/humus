// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	defaultCertsDir     = "/certs"
	defaultKeystorePass = "changeit"
	keySize             = 2048
	validityDays        = 365
)

func main() {
	certsDir := flag.String("certs-dir", defaultCertsDir, "Directory to store certificates")
	keystorePass := flag.String("keystore-pass", defaultKeystorePass, "Password for Java keystores")
	flag.Parse()

	if err := run(*certsDir, *keystorePass); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(certsDir, keystorePass string) error {
	// Check if certificates already exist
	if certificatesExist(certsDir) {
		fmt.Println("Certificates already exist. Skipping generation.")
		return nil
	}

	fmt.Println("Generating TLS certificates for Kafka mTLS...")

	// Ensure certs directory exists
	if err := os.MkdirAll(certsDir, 0755); err != nil {
		return fmt.Errorf("failed to create certs directory: %w", err)
	}

	// Generate CA
	fmt.Println("Creating CA certificate...")
	caKey, caCert, err := generateCA()
	if err != nil {
		return fmt.Errorf("failed to generate CA: %w", err)
	}

	// Write CA key and certificate
	if err := writePrivateKey(filepath.Join(certsDir, "ca-key.pem"), caKey); err != nil {
		return err
	}
	if err := writeCertificate(filepath.Join(certsDir, "ca-cert.pem"), caCert); err != nil {
		return err
	}

	// Generate server certificate
	fmt.Println("Creating Kafka broker certificate...")
	serverKey, serverCert, err := generateCertificate(caKey, caCert, "kafka", false)
	if err != nil {
		return fmt.Errorf("failed to generate server certificate: %w", err)
	}

	if err := writePrivateKey(filepath.Join(certsDir, "server-key.pem"), serverKey); err != nil {
		return err
	}
	if err := writeCertificate(filepath.Join(certsDir, "server-cert.pem"), serverCert); err != nil {
		return err
	}

	// Generate client certificate
	fmt.Println("Creating client certificate...")
	clientKey, clientCert, err := generateCertificate(caKey, caCert, "kafka-client", true)
	if err != nil {
		return fmt.Errorf("failed to generate client certificate: %w", err)
	}

	if err := writePrivateKey(filepath.Join(certsDir, "client-key.pem"), clientKey); err != nil {
		return err
	}
	if err := writeCertificate(filepath.Join(certsDir, "client-cert.pem"), clientCert); err != nil {
		return err
	}

	// Create Java keystores
	fmt.Println("Creating Java keystores...")
	if err := createJavaKeystores(certsDir, keystorePass); err != nil {
		return fmt.Errorf("failed to create Java keystores: %w", err)
	}

	// Create credential files
	if err := writeCredentialFiles(certsDir, keystorePass); err != nil {
		return err
	}

	fmt.Println("Certificate generation complete!")
	fmt.Println("Files created:")
	listCertificates(certsDir)

	return nil
}

func certificatesExist(certsDir string) bool {
	files := []string{"ca-cert.pem", "server-cert.pem", "client-cert.pem"}
	for _, file := range files {
		if _, err := os.Stat(filepath.Join(certsDir, file)); os.IsNotExist(err) {
			return false
		}
	}
	return true
}

func generateCA() (*rsa.PrivateKey, *x509.Certificate, error) {
	// Generate private key
	key, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate key: %w", err)
	}

	// Create certificate template
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "Kafka-Test-CA",
			Organization: []string{"Humus-Example"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(0, 0, validityDays),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// Create self-signed certificate
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return key, cert, nil
}

func generateCertificate(caKey *rsa.PrivateKey, caCert *x509.Certificate, commonName string, isClient bool) (*rsa.PrivateKey, *x509.Certificate, error) {
	// Generate private key
	key, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate key: %w", err)
	}

	// Create certificate template
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().Unix()),
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"Humus-Example"},
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().AddDate(0, 0, validityDays),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}

	if isClient {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}

	// Sign certificate with CA
	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	return key, cert, nil
}

func writePrivateKey(filename string, key *rsa.PrivateKey) error {
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	if err := os.WriteFile(filename, keyPEM, 0600); err != nil {
		return fmt.Errorf("failed to write private key: %w", err)
	}

	return nil
}

func writeCertificate(filename string, cert *x509.Certificate) error {
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})

	if err := os.WriteFile(filename, certPEM, 0644); err != nil {
		return fmt.Errorf("failed to write certificate: %w", err)
	}

	return nil
}

func createJavaKeystores(certsDir, keystorePass string) error {
	// Create PKCS12 file for server certificate
	p12File := filepath.Join(certsDir, "server.p12")
	cmd := exec.Command("openssl", "pkcs12", "-export",
		"-in", filepath.Join(certsDir, "server-cert.pem"),
		"-inkey", filepath.Join(certsDir, "server-key.pem"),
		"-out", p12File,
		"-name", "kafka",
		"-CAfile", filepath.Join(certsDir, "ca-cert.pem"),
		"-caname", "root",
		"-password", "pass:"+keystorePass)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create PKCS12 file: %w", err)
	}

	// Import PKCS12 into JKS keystore
	keystoreFile := filepath.Join(certsDir, "kafka.keystore.jks")
	cmd = exec.Command("keytool", "-importkeystore",
		"-deststorepass", keystorePass,
		"-destkeypass", keystorePass,
		"-destkeystore", keystoreFile,
		"-srckeystore", p12File,
		"-srcstoretype", "PKCS12",
		"-srcstorepass", keystorePass,
		"-alias", "kafka",
		"-noprompt")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create keystore: %w", err)
	}

	// Create truststore with CA certificate
	truststoreFile := filepath.Join(certsDir, "kafka.truststore.jks")
	cmd = exec.Command("keytool", "-keystore", truststoreFile,
		"-alias", "CARoot",
		"-import",
		"-file", filepath.Join(certsDir, "ca-cert.pem"),
		"-storepass", keystorePass,
		"-noprompt")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create truststore: %w", err)
	}

	// Clean up temporary PKCS12 file
	if err := os.Remove(p12File); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove temporary file %s: %v\n", p12File, err)
	}

	return nil
}

func writeCredentialFiles(certsDir, keystorePass string) error {
	credFiles := []string{"keystore_creds", "key_creds", "truststore_creds"}
	for _, file := range credFiles {
		if err := os.WriteFile(filepath.Join(certsDir, file), []byte(keystorePass+"\n"), 0600); err != nil {
			return fmt.Errorf("failed to write credential file %s: %w", file, err)
		}
	}
	return nil
}

func listCertificates(certsDir string) {
	patterns := []string{"*.pem", "*.jks"}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(certsDir, pattern))
		if err != nil {
			continue
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				continue
			}
			fmt.Printf("  %s (%d bytes)\n", filepath.Base(match), info.Size())
		}
	}
}
