---
title: 1BRC Algorithm with Local Files
description: Implement the core parsing and calculation logic using local file I/O
weight: 3
type: docs
---

Let's implement the core 1BRC algorithm using simple local file operations. This allows you to get the business logic working quickly before adding cloud storage.

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
- Reads line-by-line, never loading entire file into memory
- Aggregates on-the-fly in a map
- Handles EOF properly
- Memory-efficient even for billion-row files

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

	// Sort alphabetically by city name
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

## Implement Handler with Local File I/O

Create `onebrc/handler.go`:

```go
package onebrc

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
)

type Handler struct {
	inputFile  string
	outputFile string
	log        *slog.Logger
}

func NewHandler(inputFile, outputFile string) *Handler {
	return &Handler{
		inputFile:  inputFile,
		outputFile: outputFile,
		log:        slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

func (h *Handler) Handle(ctx context.Context) error {
	h.log.InfoContext(ctx, "starting 1BRC processing",
		slog.String("input_file", h.inputFile),
		slog.String("output_file", h.outputFile),
	)

	// 1. Open input file
	f, err := os.Open(h.inputFile)
	if err != nil {
		h.log.ErrorContext(ctx, "failed to open input file", slog.Any("error", err))
		return fmt.Errorf("open input file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			h.log.WarnContext(ctx, "failed to close input file", slog.Any("error", cerr))
		}
	}()

	// 2. Parse
	cityStats, err := Parse(bufio.NewReader(f))
	if err != nil {
		h.log.ErrorContext(ctx, "failed to parse temperature data", slog.Any("error", err))
		return fmt.Errorf("parse: %w", err)
	}

	// 3. Calculate
	results := Calculate(cityStats)

	// 4. Write results
	output := FormatResults(results)

	err = os.WriteFile(h.outputFile, []byte(output), 0644)
	if err != nil {
		h.log.ErrorContext(ctx, "failed to write results", slog.Any("error", err))
		return fmt.Errorf("write results: %w", err)
	}

	h.log.InfoContext(ctx, "1BRC processing completed successfully",
		slog.Int("cities_processed", len(cityStats)),
	)

	return nil
}
```

**Why local files first?**
- No external dependencies (MinIO, S3)
- Fast iteration cycle
- Easy to test and debug
- Proves the algorithm works
- Can refactor to cloud storage later

## Update Configuration

Now that the handler is implemented, let's wire it up through the configuration system.

First, update `config.yaml` to add the file paths:

```yaml
onebrc:
  input_file: {{env "INPUT_FILE" | default "measurements.txt"}}
  output_file: {{env "OUTPUT_FILE" | default "results.txt"}}
```

Then update `app/app.go` to define the Config struct and use the handler:

```go
package app

import (
	"context"

	"1brc-walkthrough/onebrc"
	"github.com/z5labs/humus/job"
)

type Config struct {
	OneBRC struct {
		InputFile  string `config:"input_file"`
		OutputFile string `config:"output_file"`
	} `config:"onebrc"`
}

func Init(ctx context.Context, cfg Config) (*job.App, error) {
	handler := onebrc.NewHandler(
		cfg.OneBRC.InputFile,
		cfg.OneBRC.OutputFile,
	)

	return job.NewApp(handler), nil
}
```

**How config mapping works:**
- The `onebrc:` key in YAML maps to the `OneBRC` struct field via the `config:"onebrc"` tag
- The `input_file:` and `output_file:` keys map to `InputFile` and `OutputFile` fields
- Template expressions like `{{env "INPUT_FILE"}}` allow environment variable overrides

## What's Next

Now let's test the complete job with local files to verify everything works.

[Next: Running Without OTel →]({{< ref "04-running-without-otel" >}})
