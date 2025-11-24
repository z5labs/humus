---
title: 1BRC Algorithm
description: Parsing and calculating temperature statistics
weight: 4
type: docs
---

The `onebrc` package implements the core challenge logic: parse temperature data and calculate statistics.

## Parser Implementation

Create `onebrc/parser.go`:

```go
package onebrc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type CityStats struct {
	Min   float64
	Max   float64
	Sum   float64
	Count int64
}

func Parse(r *bufio.Reader) (map[string]*CityStats, error) {
	stats := make(map[string]*CityStats)

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) > 0 {
					if err := parseLine(line, stats); err != nil {
						return nil, err
					}
				}
				break
			}
			return nil, fmt.Errorf("read line: %w", err)
		}

		if err := parseLine(line, stats); err != nil {
			return nil, err
		}
	}

	return stats, nil
}

func parseLine(line string, stats map[string]*CityStats) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	parts := strings.SplitN(line, ";", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid line format: %s", line)
	}

	city := parts[0]
	temp, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return fmt.Errorf("parse temperature: %w", err)
	}

	cityStats, exists := stats[city]
	if !exists {
		stats[city] = &CityStats{
			Min:   temp,
			Max:   temp,
			Sum:   temp,
			Count: 1,
		}
		return nil
	}

	// Update existing stats
	if temp < cityStats.Min {
		cityStats.Min = temp
	}
	if temp > cityStats.Max {
		cityStats.Max = temp
	}
	cityStats.Sum += temp
	cityStats.Count++

	return nil
}
```

**Streaming approach:**
- Reads line-by-line, never loading entire file
- Aggregates on-the-fly in a map
- Handles EOF properly

## Calculator Implementation

Create `onebrc/calculator.go`:

```go
package onebrc

import (
	"fmt"
	"sort"
	"strings"
)

type CityResult struct {
	City string
	Min  float64
	Mean float64
	Max  float64
}

func Calculate(stats map[string]*CityStats) []CityResult {
	results := make([]CityResult, 0, len(stats))

	for city, s := range stats {
		mean := s.Sum / float64(s.Count)
		results = append(results, CityResult{
			City: city,
			Min:  s.Min,
			Mean: mean,
			Max:  s.Max,
		})
	}

	// Sort alphabetically
	sort.Slice(results, func(i, j int) bool {
		return results[i].City < results[j].City
	})

	return results
}

func FormatResults(results []CityResult) string {
	if len(results) == 0 {
		return ""
	}

	var sb strings.Builder

	for _, r := range results {
		sb.WriteString(fmt.Sprintf("%s=%.1f/%.1f/%.1f\n",
			r.City, round(r.Min), round(r.Mean), round(r.Max)))
	}

	return sb.String()
}

// IEEE 754 rounding to 1 decimal place
func round(value float64) float64 {
	return float64(int(value*10+0.5)) / 10
}
```

**Output format:**
```
Abha=-23.0/18.0/59.2
Jakarta=-10.0/26.5/45.3
Tokyo=-5.2/35.6/50.1
```

One city per line: `city=min/mean/max`

## Update Handler

Now update `app/handler.go` to use the parsing and calculation logic:

```go
package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"1brc-walkthrough/onebrc"
)

type Storage interface {
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64) error
}

type Handler struct {
	storage   Storage
	bucket    string
	inputKey  string
	outputKey string
	log       *slog.Logger
}

func NewHandler(storage Storage, bucket, inputKey, outputKey string) *Handler {
	return &Handler{
		storage:   storage,
		bucket:    bucket,
		inputKey:  inputKey,
		outputKey: outputKey,
		log:       slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

func (h *Handler) Handle(ctx context.Context) error {
	h.log.InfoContext(ctx, "starting 1BRC processing",
		slog.String("bucket", h.bucket),
		slog.String("input_key", h.inputKey),
	)

	// 1. Fetch from S3
	rc, err := h.storage.GetObject(ctx, h.bucket, h.inputKey)
	if err != nil {
		h.log.ErrorContext(ctx, "failed to fetch input object", slog.Any("error", err))
		return fmt.Errorf("get object: %w", err)
	}
	defer rc.Close()

	// 2. Parse
	cityStats, err := onebrc.Parse(bufio.NewReader(rc))
	if err != nil {
		h.log.ErrorContext(ctx, "failed to parse temperature data", slog.Any("error", err))
		return fmt.Errorf("parse: %w", err)
	}

	// 3. Calculate
	results := onebrc.Calculate(cityStats)

	// 4. Write results
	output := onebrc.FormatResults(results)
	outputBytes := []byte(output)

	err = h.storage.PutObject(ctx, h.bucket, h.outputKey,
		bytes.NewReader(outputBytes), int64(len(outputBytes)))
	if err != nil {
		h.log.ErrorContext(ctx, "failed to upload results", slog.Any("error", err))
		return fmt.Errorf("put object: %w", err)
	}

	h.log.InfoContext(ctx, "1BRC processing completed successfully",
		slog.Int("cities_processed", len(cityStats)),
	)

	return nil
}
```

No changes needed to `app/app.go` - it already wires everything together correctly!

## What's Next

Now let's test the complete job with a simple dataset to verify everything works before adding observability.

[Next: Running Without OTel →]({{< ref "05-running-without-otel" >}})
