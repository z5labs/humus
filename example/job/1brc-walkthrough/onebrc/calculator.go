// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package onebrc

import (
	"fmt"
	"sort"
	"strings"
)

// CityResult holds calculated statistics for a city.
type CityResult struct {
	City string
	Min  float64
	Mean float64
	Max  float64
}

// Calculate computes final statistics and sorts results by city name.
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

	sort.Slice(results, func(i, j int) bool {
		return results[i].City < results[j].City
	})

	return results
}

// FormatResults formats results with one city per line.
// Format: City1=min/mean/max\nCity2=min/mean/max\n...
func FormatResults(results []CityResult) string {
	if len(results) == 0 {
		return ""
	}

	var sb strings.Builder

	for _, r := range results {
		sb.WriteString(fmt.Sprintf("%s=%.1f/%.1f/%.1f\n", r.City, round(r.Min), round(r.Mean), round(r.Max)))
	}

	return sb.String()
}

// round implements IEEE 754 rounding-direction "roundTowardPositive" to 1 decimal place.
func round(value float64) float64 {
	return float64(int(value*10+0.5)) / 10
}
