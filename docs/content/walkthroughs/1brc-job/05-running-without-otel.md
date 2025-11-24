---
title: Running Without OTel
description: Test your job with minimal infrastructure
weight: 5
type: docs
---

Let's verify your job works before adding the full observability stack. This approach lets you iterate quickly and debug any issues.

## Generate Test Data

First, create a simple tool to generate test data. Create `tool/main.go`:

```go
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var cities = []string{
	"Tokyo", "Jakarta", "Delhi", "Manila", "Shanghai",
	"Sao Paulo", "Mumbai", "Beijing", "Cairo", "Mexico City",
	"New York", "London", "Paris", "Moscow", "Sydney",
}

func main() {
	count := flag.Int("count", 10000, "number of measurements to generate")
	flag.Parse()

	// Connect to MinIO
	mc, err := minio.New("localhost:9000", &minio.Options{
		Creds:  credentials.NewStaticV4("minioadmin", "minioadmin", ""),
		Secure: false,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Create bucket if needed
	ctx := context.Background()
	exists, err := mc.BucketExists(ctx, "onebrc")
	if err != nil {
		log.Fatal(err)
	}
	if !exists {
		err = mc.MakeBucket(ctx, "onebrc", minio.MakeBucketOptions{})
		if err != nil {
			log.Fatal(err)
		}
		log.Println("Created bucket: onebrc")
	}

	// Generate data
	log.Printf("Generating %d measurements...\n", *count)
	var buf bytes.Buffer
	for i := 0; i < *count; i++ {
		city := cities[rand.Intn(len(cities))]
		temp := -20.0 + rand.Float64()*70.0 // -20 to 50°C
		buf.WriteString(fmt.Sprintf("%s;%.1f\n", city, temp))
	}

	// Upload to MinIO
	data := buf.Bytes()
	_, err = mc.PutObject(ctx, "onebrc", "measurements.txt",
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Uploaded %d bytes to onebrc/measurements.txt\n", len(data))
}
```

## Build and Generate Data

```bash
# Make sure MinIO is running
podman ps

# Generate test data (10K measurements for quick testing)
cd tool
go run . -count 10000
```

You should see:

```
Created bucket: onebrc
Generating 10000 measurements...
Uploaded 123456 bytes to onebrc/measurements.txt
```

## Verify Data in MinIO Console

1. Open http://localhost:9001
2. Login with minioadmin/minioadmin
3. Browse the `onebrc` bucket
4. You should see `measurements.txt`
5. Click to preview - you'll see lines like:
   ```
   Tokyo;35.6
   Jakarta;-6.2
   Delhi;28.7
   ```

## Build and Run Your Job

```bash
# Return to project root
cd ..

# Ensure dependencies are installed
go mod tidy

# Run the job
go run .
```

You should see simple log output to stdout:

```
...job starting...
...job completed successfully...
```

Since we haven't added observability yet, the logs are minimal. That's expected!

## Verify Results

Check that the job created results:

1. Open MinIO Console: http://localhost:9001
2. Browse the `onebrc` bucket
3. You should now see both files:
   - `measurements.txt` (input)
   - `results.txt` (output)
4. Download or preview `results.txt`

Expected format:

```
Beijing=-19.5/16.3/49.8
Cairo=-18.2/17.9/48.5
Delhi=-17.9/15.8/47.3
Jakarta=-19.8/14.2/49.1
...
```

Each line shows: `city=min/mean/max`

## Test With Larger Dataset

Try a larger dataset to see performance:

```bash
cd tool
go run . -count 1000000  # 1 million measurements
cd ..
go run .
```

This will take longer but you can verify the job handles larger files.

## What If It Doesn't Work?

**Job crashes or errors:**
- Check that MinIO is running: `podman ps`
- Verify bucket exists in MinIO Console
- Check config.yaml has correct MinIO endpoint

**No results.txt created:**
- Check for error messages in stdout
- Verify measurements.txt exists in MinIO
- Try with a smaller dataset first (1000 rows)

**Empty results.txt:**
- Check that measurements.txt has valid data
- Verify format is `city;temperature` (semicolon separator)

## What's Next

Your job works! Now let's add the full LGTM observability stack so you can see traces, metrics, and logs in Grafana.

[Next: Infrastructure Setup →]({{< ref "06-infrastructure" >}})
