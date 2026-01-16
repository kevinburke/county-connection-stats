package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultExtractRoutes = "4"
	defaultDataDir       = "var/historic/county-connection"
)

var (
	dataDir       = flag.String("data", defaultDataDir, "Directory containing historic GTFS data")
	routesFlag    = flag.String("routes", defaultExtractRoutes, "Comma-separated list of route IDs to extract")
	agency        = flag.String("agency", "", "Agency prefix (e.g., 'CC' for 'CC:4')")
	specificMonth = flag.String("month", "", "Extract a single month (YYYY-MM)")
	outputPath    = flag.String("out", "var/extracted/stop_observations-filtered.txt", "Output file path")
)

func main() {
	flag.Parse()

	targetRoutes := parseRouteList(*routesFlag)
	if len(targetRoutes) == 0 {
		slog.Error("No routes specified")
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(*outputPath), 0o755); err != nil {
		slog.Error("Failed to create output directory", "error", err)
		os.Exit(1)
	}

	months, err := getMonthsToProcess(*dataDir, *specificMonth)
	if err != nil {
		slog.Error("Failed to list months", "error", err)
		os.Exit(1)
	}
	if len(months) == 0 {
		slog.Error("No months found", "dataDir", *dataDir)
		os.Exit(1)
	}

	outFile, err := os.Create(*outputPath)
	if err != nil {
		slog.Error("Failed to create output file", "error", err)
		os.Exit(1)
	}
	defer outFile.Close()

	writer := csv.NewWriter(outFile)
	defer writer.Flush()

	var wroteHeader bool
	totalRecords := 0

	for _, month := range months {
		filePath := filepath.Join(*dataDir, month, "stop_observations.txt")
		file, err := os.Open(filePath)
		if err != nil {
			slog.Warn("Skipping month (stop_observations missing)", "month", month, "error", err)
			continue
		}

		reader := csv.NewReader(file)
		reader.TrimLeadingSpace = true

		header, err := reader.Read()
		if err != nil {
			file.Close()
			slog.Warn("Skipping month (bad header)", "month", month, "error", err)
			continue
		}

		colIndex := make(map[string]int)
		for i, col := range header {
			colIndex[col] = i
		}

		routeIdx, ok := colIndex["route_id"]
		if !ok {
			file.Close()
			slog.Warn("Skipping month (route_id column missing)", "month", month)
			continue
		}

		if !wroteHeader {
			if err := writer.Write(header); err != nil {
				file.Close()
				slog.Error("Failed to write header", "error", err)
				os.Exit(1)
			}
			wroteHeader = true
		}

		monthRecords := 0
		for {
			record, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				// skip malformed line
				continue
			}

			routeVal := valueAt(record, routeIdx)
			if !routeMatches(routeVal, targetRoutes, *agency) {
				continue
			}

			if err := writer.Write(record); err != nil {
				file.Close()
				slog.Error("Failed to write record", "error", err)
				os.Exit(1)
			}
			monthRecords++
			totalRecords++
		}

		file.Close()
		slog.Info("Month extracted", "month", month, "records", monthRecords)
	}

	if err := writer.Error(); err != nil {
		slog.Error("Failed to finalize output", "error", err)
		os.Exit(1)
	}

	slog.Info("Extraction complete", "routes", strings.Join(targetRoutes, ","), "records", totalRecords, "output", *outputPath)
}

func getMonthsToProcess(dataDir, specificMonth string) ([]string, error) {
	if specificMonth != "" {
		monthPath := filepath.Join(dataDir, specificMonth)
		if _, err := os.Stat(monthPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("month %s not found", specificMonth)
		}
		return []string{specificMonth}, nil
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}

	var months []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && len(name) == 7 && name[4] == '-' {
			months = append(months, name)
		}
	}
	sort.Strings(months)
	return months, nil
}

func parseRouteList(raw string) []string {
	parts := strings.Split(raw, ",")
	var routes []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			routes = append(routes, part)
		}
	}
	return routes
}

func routeMatches(routeValue string, targets []string, agency string) bool {
	if routeValue == "" {
		return false
	}
	for _, target := range targets {
		if routeValue == target {
			return true
		}
		if agency != "" && routeValue == agency+":"+target {
			return true
		}
	}
	return false
}

func valueAt(record []string, idx int) string {
	if idx >= 0 && idx < len(record) {
		return record[idx]
	}
	return ""
}
