---
title: 1BRC Algorithm
description: Parsing and calculating temperature statistics
weight: 5
type: docs
---

The `onebrc` package implements the core challenge logic: parse temperature data and calculate statistics.

## Parser Implementation

```go
// parser.go
package onebrc

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

```go
// calculator.go
package onebrc

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

## Next Steps

Continue to: [Observability]({{< ref "06-observability" >}})
