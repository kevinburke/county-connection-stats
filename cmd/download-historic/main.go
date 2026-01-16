// download-historic downloads historic GTFS data from the 511.org API.
//
// It fetches monthly archives from March 2022 onwards, including the
// stop_observations.txt file that contains observed real-time arrival times.
// Data is saved to var/historic/county-connection/ by default.
//
// Usage:
//
//	export API_511_KEY=your_key_here
//	go run ./cmd/download-historic
//
// Options:
//
//	-start     Start month (default: 2022-03)
//	-end       End month (default: current month)
//	-output    Output directory
//	-version   Print version and exit
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/kevinburke/rest/restclient"
)

const (
	version         = "0.1.0"
	apiBaseURL      = "https://api.511.org/transit/datafeeds"
	operatorID      = "RG" // County Connection operator ID
	historicDataDir = "var/historic/county-connection"
)

var (
	versionFlag = flag.Bool("version", false, "Print version and exit")
	startMonth  = flag.String("start", "2022-03", "Start month in YYYY-MM format (default: 2022-03)")
	endMonth    = flag.String("end", "", "End month in YYYY-MM format (default: current month)")
	outputDir   = flag.String("output", historicDataDir, "Output directory for downloaded data")
)

func main() {
	flag.Parse()

	if *versionFlag {
		fmt.Printf("download-historic version %s\n", version)
		os.Exit(0)
	}

	// Get API key from environment
	apiKey := os.Getenv("API_511_KEY")
	if apiKey == "" {
		slog.Error("API_511_KEY environment variable not set")
		slog.Info("Get your free API key at: https://511.org/open-data/token")
		os.Exit(1)
	}

	// Parse start date
	start, err := time.Parse("2006-01", *startMonth)
	if err != nil {
		slog.Error("Invalid start month format", "start", *startMonth, "error", err)
		slog.Info("Use YYYY-MM format, e.g., 2022-03")
		os.Exit(1)
	}

	// Parse end date (default to current month)
	var end time.Time
	if *endMonth == "" {
		now := time.Now()
		end = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		end, err = time.Parse("2006-01", *endMonth)
		if err != nil {
			slog.Error("Invalid end month format", "end", *endMonth, "error", err)
			slog.Info("Use YYYY-MM format, e.g., 2024-12")
			os.Exit(1)
		}
	}

	// Create output directory
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		slog.Error("Failed to create output directory", "dir", *outputDir, "error", err)
		os.Exit(1)
	}

	slog.Info("Starting historic data download",
		"start", start.Format("2006-01"),
		"end", end.Format("2006-01"),
		"outputDir", *outputDir)

	// Download each month
	client := &http.Client{
		Transport: restclient.DefaultTransport,
		Timeout:   5 * time.Minute,
	}

	current := start
	successCount := 0
	skipCount := 0
	errorCount := 0

	for !current.After(end) {
		monthStr := current.Format("2006-01")
		historicParam := monthStr + "-so" // Append -so for stop_observations

		// Check if already downloaded
		monthDir := filepath.Join(*outputDir, monthStr)
		if _, err := os.Stat(monthDir); err == nil {
			slog.Info("Month already downloaded, skipping", "month", monthStr)
			skipCount++
			current = current.AddDate(0, 1, 0)
			continue
		}

		slog.Info("Downloading", "month", monthStr)

		if err := downloadMonth(client, apiKey, historicParam, monthDir); err != nil {
			slog.Error("Failed to download month", "month", monthStr, "error", err)
			errorCount++
		} else {
			slog.Info("Successfully downloaded", "month", monthStr)
			successCount++
		}

		// Move to next month
		current = current.AddDate(0, 1, 0)

		// Be nice to the API - rate limit
		if !current.After(end) {
			time.Sleep(2 * time.Second)
		}
	}

	slog.Info("Download complete",
		"success", successCount,
		"skipped", skipCount,
		"errors", errorCount)

	if errorCount > 0 {
		os.Exit(1)
	}
}

// downloadMonth downloads and extracts the historic GTFS data for a given month
func downloadMonth(client *http.Client, apiKey, historicParam, outputDir string) error {
	// Build request URL
	url := fmt.Sprintf("%s?api_key=%s&operator_id=%s&historic=%s",
		apiBaseURL, apiKey, operatorID, historicParam)

	// Make request
	slog.Debug("Fetching", "historic", historicParam)
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Create temp file for zip
	tmpFile, err := os.CreateTemp("", "gtfs-*.zip")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	// Download to temp file
	size, err := io.Copy(tmpFile, resp.Body)
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to download: %w", err)
	}
	tmpFile.Close()

	slog.Debug("Downloaded zip file", "size", size, "path", tmpPath)

	// Extract zip file
	if err := extractZip(tmpPath, outputDir); err != nil {
		return fmt.Errorf("failed to extract zip: %w", err)
	}

	slog.Debug("Extracted to", "dir", outputDir)
	return nil
}

// extractZip extracts a zip file to the specified directory
func extractZip(zipPath, destDir string) error {
	// Open zip file
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	// Create destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	// Extract each file
	for _, f := range r.File {
		if err := extractZipFile(f, destDir); err != nil {
			return err
		}
	}

	return nil
}

// extractZipFile extracts a single file from a zip archive
func extractZipFile(f *zip.File, destDir string) error {
	// Open file in zip
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	// Build destination path
	path := filepath.Join(destDir, f.Name)

	// Check for directory traversal
	if !filepath.HasPrefix(path, filepath.Clean(destDir)+string(os.PathSeparator)) {
		return fmt.Errorf("invalid file path: %s", f.Name)
	}

	// Create directory if needed
	if f.FileInfo().IsDir() {
		return os.MkdirAll(path, f.Mode())
	}

	// Create file
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	outFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return err
	}
	defer outFile.Close()

	// Copy contents
	_, err = io.Copy(outFile, rc)
	return err
}
