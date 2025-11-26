// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var cities = []string{
	"Tokyo", "Jakarta", "Delhi", "Manila", "Shanghai",
	"Sao Paulo", "Mumbai", "Beijing", "Cairo", "Mexico City",
	"New York", "London", "Paris", "Moscow", "Sydney",
}

func main() {
	var (
		count      = flag.Int("count", 10000, "number of measurements to generate")
		endpoint   = flag.String("endpoint", "localhost:9000", "MinIO endpoint")
		accessKey  = flag.String("access-key", "minioadmin", "MinIO access key")
		secretKey  = flag.String("secret-key", "minioadmin", "MinIO secret key")
		bucket     = flag.String("bucket", "onebrc", "MinIO bucket name")
		objectKey  = flag.String("key", "measurements.txt", "Object key for the dataset")
		outputFile = flag.String("output", "", "Output to file instead of MinIO (optional)")
	)
	flag.Parse()

	// Generate data
	log.Printf("Generating %d measurements...\n", *count)
	var buf bytes.Buffer
	for i := 0; i < *count; i++ {
		city := cities[rand.Intn(len(cities))]
		temp := -20.0 + rand.Float64()*70.0 // -20 to 50°C
		buf.WriteString(fmt.Sprintf("%s;%.1f\n", city, temp))
	}
	data := buf.Bytes()

	// Output to file if requested
	if *outputFile != "" {
		if err := os.WriteFile(*outputFile, data, 0644); err != nil {
			log.Fatal(err)
		}
		log.Printf("Generated %d bytes to %s\n", len(data), *outputFile)
		return
	}

	// Connect to MinIO
	mc, err := minio.New(*endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(*accessKey, *secretKey, ""),
		Secure: false,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Create bucket if needed
	ctx := context.Background()
	exists, err := mc.BucketExists(ctx, *bucket)
	if err != nil {
		log.Fatal(err)
	}
	if !exists {
		err = mc.MakeBucket(ctx, *bucket, minio.MakeBucketOptions{})
		if err != nil {
			log.Fatal(err)
		}
		log.Println("Created bucket:", *bucket)
	}

	// Upload to MinIO
	_, err = mc.PutObject(ctx, *bucket, *objectKey,
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Uploaded %d bytes to %s/%s\n", len(data), *bucket, *objectKey)
}
