---
title: Running With Local Files
description: Test your job quickly with local file I/O
weight: 4
type: docs
---

Let's verify your job works with simple local files before adding cloud storage and observability. This approach lets you iterate quickly and debug any issues.

## Generate Test Data

Create a simple tool to generate test data locally. Create `tool/main.go`:

```go
package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
)

var cities = []string{
	"Tokyo", "Jakarta", "Delhi", "Manila", "Shanghai",
	"Sao Paulo", "Mumbai", "Beijing", "Cairo", "Mexico City",
	"New York", "London", "Paris", "Moscow", "Sydney",
}

func main() {
	count := flag.Int("count", 10000, "number of measurements to generate")
	output := flag.String("output", "measurements.txt", "output file path")
	flag.Parse()

	log.Printf("Generating %d measurements...\n", *count)

	f, err := os.Create(*output)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	for i := 0; i < *count; i++ {
		city := cities[rand.Intn(len(cities))]
		temp := -20.0 + rand.Float64()*70.0 // -20 to 50°C
		fmt.Fprintf(f, "%s;%.1f\n", city, temp)
	}

	log.Printf("Generated %s successfully\n", *output)
}
```

## Build and Generate Data

```bash
# Generate test data (10K measurements for quick testing)
cd tool
go run . -count 10000 -output ../measurements.txt
```

You should see:

```
Generating 10000 measurements...
Generated measurements.txt successfully
```

## Verify Test Data

Check the generated file:

```bash
cd ..
head -5 measurements.txt
```

You should see lines like:
```
Tokyo;35.6
Jakarta;-6.2
Delhi;28.7
Manila;32.1
Shanghai;18.9
```

## Build and Run Your Job

```bash
# Ensure dependencies are installed
go mod tidy

# Run the job
go run .
```

You should see simple log output:

```json
{"time":"2024-11-23T22:50:00Z","level":"INFO","msg":"starting 1BRC processing","input_file":"measurements.txt","output_file":"results.txt"}
{"time":"2024-11-23T22:50:00Z","level":"INFO","msg":"1BRC processing completed successfully","cities_processed":15}
```

**That's it!** Your job runs in less than a second and processes the data.

## Verify Results

Check the output file:

```bash
cat results.txt
```

Expected format:

```
Beijing=-19.5/16.3/49.8
Cairo=-18.2/17.9/48.5
Delhi=-17.9/15.8/47.3
Jakarta=-19.8/14.2/49.1
London=-19.3/15.4/48.9
Mexico City=-18.7/16.1/49.5
Moscow=-19.1/14.8/49.2
Mumbai=-19.6/15.7/48.8
New York=-18.9/15.3/49.1
Paris=-19.4/16.2/49.3
Sao Paulo=-18.5/15.6/48.7
Shanghai=-19.2/15.9/49.4
Sydney=-18.8/16.5/49.6
Tokyo=-19.7/15.1/48.6
```

Each line shows: `city=min/mean/max`

## Test With Larger Dataset

Try a larger dataset to see performance:

```bash
cd tool
go run . -count 1000000 -output ../measurements.txt  # 1 million measurements
cd ..
time go run .
```

This will take a bit longer but you can verify the job handles larger files efficiently.

## What If It Doesn't Work?

**Job crashes or errors:**
- Check that `measurements.txt` exists in the project root
- Verify the file has valid data in `city;temperature` format
- Check config.yaml has correct file paths

**No results.txt created:**
- Check for error messages in stdout
- Try with a smaller dataset first (1000 rows)
- Verify write permissions in the directory

**Empty results.txt:**
- Check that measurements.txt has valid data
- Verify format is `city;temperature` (semicolon separator)
- Check for parsing errors in the logs

## Why Start With Local Files?

**Benefits:**
- Zero infrastructure setup required
- Instant feedback loop
- Easy to debug with local files
- Proves the algorithm works correctly
- Can add cloud storage later without changing the core logic

## What's Next

Your job works with local files! Now let's refactor it to use MinIO (S3-compatible storage) so you can process files in the cloud.

[Next: MinIO Integration →]({{< ref "05-minio-integration" >}})
