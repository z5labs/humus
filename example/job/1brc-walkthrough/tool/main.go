// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const weatherStationsURL = "https://raw.githubusercontent.com/gunnarmorling/1brc/main/data/weather_stations.csv"

// WeatherStation represents a city and its average temperature.
type WeatherStation struct {
	Name    string
	AvgTemp float64
}

func main() {
	var (
		count      = flag.Int("count", 1000000000, "Number of measurements to generate (default: 1 billion)")
		workers    = flag.Int("workers", runtime.NumCPU(), "Number of concurrent workers")
		endpoint   = flag.String("endpoint", "localhost:9000", "MinIO endpoint")
		accessKey  = flag.String("access-key", "minioadmin", "MinIO access key")
		secretKey  = flag.String("secret-key", "minioadmin", "MinIO secret key")
		bucket     = flag.String("bucket", "onebrc", "MinIO bucket name")
		objectKey  = flag.String("key", "measurements.txt", "Object key for the dataset")
		outputFile = flag.String("output", "", "Output to file instead of MinIO (optional)")
	)
	flag.Parse()

	log.Println("Fetching weather stations data from 1BRC repository...")
	stations, err := fetchWeatherStations()
	if err != nil {
		log.Fatalf("failed to fetch weather stations: %v", err)
	}
	log.Printf("Loaded %d weather stations", len(stations))

	if *outputFile != "" {
		if err := generateToFile(*outputFile, *count, *workers, stations); err != nil {
			log.Fatalf("failed to generate to file: %v", err)
		}
		log.Printf("successfully generated %d measurements to %s", *count, *outputFile)
		return
	}

	if err := generateToMinio(*endpoint, *accessKey, *secretKey, *bucket, *objectKey, *count, *workers, stations); err != nil {
		log.Fatalf("failed to upload to MinIO: %v", err)
	}

	log.Printf("successfully uploaded %d measurements to MinIO bucket %s (key: %s)", *count, *bucket, *objectKey)
}

func fetchWeatherStations() (stations []WeatherStation, err error) {
	resp, err := http.Get(weatherStationsURL)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close response body: %w", cerr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}

		parts := strings.SplitN(line, ";", 2)
		if len(parts) != 2 {
			continue
		}

		avgTemp, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			continue
		}

		stations = append(stations, WeatherStation{
			Name:    parts[0],
			AvgTemp: avgTemp,
		})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error: %w", err)
	}

	if len(stations) == 0 {
		return nil, fmt.Errorf("no weather stations found")
	}

	return stations, nil
}

func generateToFile(filename string, count, workers int, stations []WeatherStation) (err error) {
	log.Printf("Generating %d measurements to file using %d workers...", count, workers)

	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close file: %w", cerr)
		}
	}()

	w := bufio.NewWriterSize(f, 1024*1024)

	if err := generateMeasurementsConcurrent(w, count, workers, stations); err != nil {
		return err
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush writer: %w", err)
	}

	return nil
}

func generateToMinio(endpoint, accessKey, secretKey, bucket, objectKey string, count, workers int, stations []WeatherStation) error {
	ctx := context.Background()

	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	if err != nil {
		return fmt.Errorf("create minio client: %w", err)
	}

	exists, err := mc.BucketExists(ctx, bucket)
	if err != nil {
		return fmt.Errorf("check bucket exists: %w", err)
	}

	if !exists {
		if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("create bucket: %w", err)
		}
		log.Printf("Created bucket: %s", bucket)
	}

	log.Printf("Generating %d measurements using %d workers and uploading to MinIO...", count, workers)

	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create pipe: %w", err)
	}
	defer func() {
		_ = pr.Close()
	}()

	uploadCtx, uploadCancel := context.WithCancel(ctx)
	defer uploadCancel()

	errCh := make(chan error, 1)
	go func() {
		defer func() {
			_ = pw.Close()
		}()
		w := bufio.NewWriterSize(pw, 1024*1024)
		err := generateMeasurementsConcurrent(w, count, workers, stations)
		if err != nil {
			uploadCancel() // Cancel the upload if generation fails
			errCh <- err
			return
		}
		if err := w.Flush(); err != nil {
			uploadCancel()
			errCh <- err
			return
		}
		errCh <- nil
	}()

	_, err = mc.PutObject(uploadCtx, bucket, objectKey, pr, -1, minio.PutObjectOptions{
		ContentType: "text/plain",
	})
	if err != nil {
		return fmt.Errorf("upload to minio: %w", err)
	}

	if err := <-errCh; err != nil {
		return fmt.Errorf("generate measurements: %w", err)
	}

	log.Println("Upload completed successfully")
	return nil
}

func generateMeasurementsConcurrent(w io.Writer, count, workers int, stations []WeatherStation) error {
	batchSize := count / workers
	if batchSize == 0 {
		batchSize = count
		workers = 1
	}

	// Channel for batches of generated lines
	lineCh := make(chan []byte, workers*2)
	var wg sync.WaitGroup
	var generated atomic.Int64
	
	// Progress reporter
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				current := generated.Load()
				pct := float64(current) / float64(count) * 100
				log.Printf("Progress: %d/%d (%.1f%%)", current, count, pct)
			case <-done:
				return
			}
		}
	}()

	// Writer goroutine - serializes output
	writerDone := make(chan error, 1)
	go func() {
		for batch := range lineCh {
			if _, err := w.Write(batch); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()

	// Worker goroutines - generate data in parallel
	for i := 0; i < workers; i++ {
		wg.Add(1)
		start := i * batchSize
		end := start + batchSize
		if i == workers-1 {
			end = count // Last worker handles remainder
		}

		go func(start, end int) {
			defer wg.Done()
			
			rng := rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(start)))
			var buf strings.Builder
			buf.Grow(1024 * 1024) // 1MB buffer

			for j := start; j < end; j++ {
				station := stations[rng.Intn(len(stations))]
				variation := rng.NormFloat64() * 10.0
				temp := station.AvgTemp + variation

				fmt.Fprintf(&buf, "%s;%.1f\n", station.Name, temp)

				// Flush when buffer gets large
				if buf.Len() >= 1024*1024 {
					lineCh <- []byte(buf.String())
					buf.Reset()
					buf.Grow(1024 * 1024)
				}
			}

			// Flush remaining
			if buf.Len() > 0 {
				lineCh <- []byte(buf.String())
			}

			// Batch increment after loop for better performance
			generated.Add(int64(end - start))
		}(start, end)
	}

	// Wait for all workers and close channel
	wg.Wait()
	close(lineCh)
	close(done)

	// Wait for writer
	if err := <-writerDone; err != nil {
		return err
	}

	log.Printf("Progress: %d/%d (100.0%%)", count, count)
	return nil
}
