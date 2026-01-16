// inspect-data shows a sample of the historic GTFS data structure.
//
// This utility helps understand what columns and data are actually present
// in the stop_observations.txt file.
//
// Usage:
//
//	# Inspect all data for a month
//	go run ./cmd/inspect-data -month 2024-01
//
//	# Filter by agency
//	go run ./cmd/inspect-data -month 2024-01 -agency CC
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

var (
	dataDir = flag.String("data", "var/historic/county-connection", "Directory containing historic GTFS data")
	month   = flag.String("month", "", "Month to inspect (YYYY-MM)")
	limit   = flag.Int("limit", 20, "Number of rows to show")
	agency  = flag.String("agency", "", "Filter by agency (e.g., 'CC')")
)

func main() {
	flag.Parse()

	if *month == "" {
		slog.Error("Please specify a month with -month")
		os.Exit(1)
	}

	monthDir := filepath.Join(*dataDir, *month)
	if _, err := os.Stat(monthDir); os.IsNotExist(err) {
		slog.Error("Month directory does not exist", "path", monthDir)
		os.Exit(1)
	}

	// Check what files are present
	entries, err := os.ReadDir(monthDir)
	if err != nil {
		slog.Error("Failed to read directory", "error", err)
		os.Exit(1)
	}

	fmt.Printf("Files in %s:\n", monthDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			info, _ := entry.Info()
			fmt.Printf("  %s (%d bytes)\n", entry.Name(), info.Size())
		}
	}
	fmt.Println()

	// Inspect stop_observations.txt
	obsPath := filepath.Join(monthDir, "stop_observations.txt")
	if _, err := os.Stat(obsPath); err == nil {
		fmt.Println("=== stop_observations.txt ===")
		if *agency != "" {
			fmt.Printf("(filtered by agency: %s)\n", *agency)
		}
		inspectCSV(obsPath, *limit, *agency)
		fmt.Println()
	}

	// Inspect trips.txt
	tripsPath := filepath.Join(monthDir, "trips.txt")
	if _, err := os.Stat(tripsPath); err == nil {
		fmt.Println("=== trips.txt (sample) ===")
		if *agency != "" {
			fmt.Printf("(filtered by agency: %s)\n", *agency)
		}
		inspectCSV(tripsPath, 10, *agency)
		fmt.Println()
	}

	// Inspect routes.txt
	routesPath := filepath.Join(monthDir, "routes.txt")
	if _, err := os.Stat(routesPath); err == nil {
		fmt.Println("=== routes.txt ===")
		if *agency != "" {
			fmt.Printf("(filtered by agency: %s)\n", *agency)
		}
		inspectCSV(routesPath, -1, *agency) // Show all routes
		fmt.Println()
	}
}

func inspectCSV(path string, limit int, agencyFilter string) {
	file, err := os.Open(path)
	if err != nil {
		slog.Error("Failed to open file", "path", path, "error", err)
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	// Read header
	header, err := reader.Read()
	if err != nil {
		slog.Error("Failed to read header", "error", err)
		return
	}

	fmt.Printf("Columns: %s\n", strings.Join(header, ", "))
	fmt.Println()

	// Find agency_id column index if filtering
	agencyColIdx := -1
	if agencyFilter != "" {
		for i, col := range header {
			if col == "agency_id" {
				agencyColIdx = i
				break
			}
		}
		if agencyColIdx == -1 {
			fmt.Printf("Warning: agency_id column not found, showing all rows\n")
		}
	}

	// Calculate column widths for pretty printing
	widths := make([]int, len(header))
	for i, col := range header {
		widths[i] = len(col)
		if widths[i] > 30 {
			widths[i] = 30
		}
	}

	// Print header row
	for i, col := range header {
		fmt.Printf("%-*s  ", widths[i], truncate(col, widths[i]))
	}
	fmt.Println()
	for i := range header {
		fmt.Print(strings.Repeat("-", widths[i]) + "  ")
	}
	fmt.Println()

	// Read and print data rows
	count := 0
	totalRead := 0
	for {
		if limit >= 0 && count >= limit {
			break
		}

		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			slog.Warn("Error reading row", "error", err)
			continue
		}

		totalRead++

		// Apply agency filter if specified
		if agencyFilter != "" && agencyColIdx >= 0 && agencyColIdx < len(record) {
			if record[agencyColIdx] != agencyFilter {
				continue
			}
		}

		for i, val := range record {
			if i < len(widths) {
				fmt.Printf("%-*s  ", widths[i], truncate(val, widths[i]))
			}
		}
		fmt.Println()
		count++
	}

	if agencyFilter != "" {
		fmt.Printf("\n(showing %d matching rows out of %d total)\n", count, totalRead)
	} else if limit >= 0 && limit < count {
		fmt.Printf("\n... (showing first %d rows)\n", limit)
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
