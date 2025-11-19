#!/bin/sh
# Script to generate TLS certificates for Kafka mTLS example
# This script creates a CA, server certificate (for Kafka), and client certificate

set -e

CERTS_DIR="/certs"

# Check if certificates already exist
if [ -f "$CERTS_DIR/ca-cert.pem" ] && [ -f "$CERTS_DIR/server-cert.pem" ] && [ -f "$CERTS_DIR/client-cert.pem" ]; then
    echo "Certificates already exist. Skipping generation."
    exit 0
fi

echo "Generating TLS certificates for Kafka mTLS..."

cd "$CERTS_DIR"

# Generate CA private key and certificate
echo "Creating CA certificate..."
openssl req -new -x509 -days 365 \
    -keyout ca-key.pem \
    -out ca-cert.pem \
    -subj "/CN=Kafka-Test-CA/O=Humus-Example" \
    -nodes

# Generate server (Kafka broker) private key and certificate request
echo "Creating Kafka broker certificate..."
openssl req -new \
    -keyout server-key.pem \
    -out server-req.pem \
    -subj "/CN=kafka/O=Humus-Example" \
    -nodes

# Sign server certificate with CA
openssl x509 -req \
    -in server-req.pem \
    -CA ca-cert.pem \
    -CAkey ca-key.pem \
    -CAcreateserial \
    -out server-cert.pem \
    -days 365 \
    -extensions v3_req

# Generate client private key and certificate request
echo "Creating client certificate..."
openssl req -new \
    -keyout client-key.pem \
    -out client-req.pem \
    -subj "/CN=kafka-client/O=Humus-Example" \
    -nodes

# Sign client certificate with CA
openssl x509 -req \
    -in client-req.pem \
    -CA ca-cert.pem \
    -CAkey ca-key.pem \
    -CAcreateserial \
    -out client-cert.pem \
    -days 365

# Create Java keystores for Kafka (required by Kafka broker)
echo "Creating Java keystores..."

# Password for keystores
KEYSTORE_PASS="changeit"

# Create server keystore
openssl pkcs12 -export \
    -in server-cert.pem \
    -inkey server-key.pem \
    -out server.p12 \
    -name kafka \
    -CAfile ca-cert.pem \
    -caname root \
    -password pass:$KEYSTORE_PASS

keytool -importkeystore \
    -deststorepass $KEYSTORE_PASS \
    -destkeypass $KEYSTORE_PASS \
    -destkeystore kafka.keystore.jks \
    -srckeystore server.p12 \
    -srcstoretype PKCS12 \
    -srcstorepass $KEYSTORE_PASS \
    -alias kafka \
    -noprompt

# Create truststore with CA certificate
keytool -keystore kafka.truststore.jks \
    -alias CARoot \
    -import \
    -file ca-cert.pem \
    -storepass $KEYSTORE_PASS \
    -noprompt

# Create credential files for Kafka
echo "$KEYSTORE_PASS" > keystore_creds
echo "$KEYSTORE_PASS" > key_creds
echo "$KEYSTORE_PASS" > truststore_creds

# Set proper permissions
chmod 644 *.pem *.jks *_creds
rm -f server.p12 server-req.pem client-req.pem

echo "Certificate generation complete!"
echo "Files created:"
ls -lh "$CERTS_DIR"/*.pem "$CERTS_DIR"/*.jks
