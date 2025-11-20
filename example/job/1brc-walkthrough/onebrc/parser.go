// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package onebrc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// CityStats holds temperature measurements for a city.
type CityStats struct {
	Min   float64
	Max   float64
	Sum   float64
	Count int64
}

// Parse reads temperature data in "city;temp" format and returns aggregated statistics.
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
