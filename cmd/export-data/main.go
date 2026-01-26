// export-data consolidates and exports tracking data in shareable formats.
//
// It converts the TSV tracking data to CSV and creates gzipped versions
// of both the live tracking data and historic stop observations.
//
// Usage:
//
//	go run ./cmd/export-data
//	go run ./cmd/export-data -output-dir exports
//
// Options:
//
//	-tracking-file    Vehicle tracking TSV file (default: vehicle-tracking.tsv)
//	-historic-file    Historic stop observations file (default: var/extracted/cc45-stop-observations.txt)
//	-output-dir       Output directory for exports (default: exports)
//	-version          Print version and exit
package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const version = "0.1.0"

var (
	trackingFile = flag.String("tracking-file", "vehicle-tracking.tsv", "Vehicle tracking TSV file")
	historicFile = flag.String("historic-file", "var/extracted/cc45-stop-observations.txt", "Historic stop observations file")
	outputDir    = flag.String("output-dir", "exports", "Output directory for exports")
	versionFlag  = flag.Bool("version", false, "Print version and exit")
)

func main() {
	flag.Parse()

	if *versionFlag {
		fmt.Printf("export-data version %s\n", version)
		os.Exit(0)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create output directory
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		logger.Error("failed to create output directory", "dir", *outputDir, "error", err)
		os.Exit(1)
	}

	timestamp := time.Now().Format("2006-01-02")

	// Export live tracking data
	if err := exportTrackingData(logger, *trackingFile, *outputDir, timestamp); err != nil {
		logger.Error("failed to export tracking data", "error", err)
		os.Exit(1)
	}

	// Export historic data
	if err := exportHistoricData(logger, *historicFile, *outputDir, timestamp); err != nil {
		logger.Error("failed to export historic data", "error", err)
		// Don't exit - historic file might not exist
	}

	logger.Info("export complete", "dir", *outputDir)
}

func exportTrackingData(logger *slog.Logger, inputPath, outputDir, timestamp string) error {
	input, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer input.Close()

	// Count lines for progress
	scanner := bufio.NewScanner(input)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
	}
	input.Seek(0, 0)

	// Create CSV output
	csvName := fmt.Sprintf("vehicle-tracking-%s.csv", timestamp)
	csvPath := filepath.Join(outputDir, csvName)
	csvFile, err := os.Create(csvPath)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer csvFile.Close()

	// Convert TSV to CSV
	scanner = bufio.NewScanner(input)
	writer := bufio.NewWriter(csvFile)
	for scanner.Scan() {
		line := scanner.Text()
		// Replace tabs with commas
		csvLine := strings.ReplaceAll(line, "\t", ",")
		writer.WriteString(csvLine)
		writer.WriteString("\n")
	}
	writer.Flush()

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan input: %w", err)
	}

	logger.Info("wrote CSV", "file", csvPath, "lines", lineCount)

	// Create gzipped version
	gzPath := csvPath + ".gz"
	if err := gzipFile(csvPath, gzPath); err != nil {
		return fmt.Errorf("gzip csv: %w", err)
	}

	gzInfo, _ := os.Stat(gzPath)
	csvInfo, _ := os.Stat(csvPath)
	logger.Info("wrote gzipped CSV",
		"file", gzPath,
		"original_size", formatBytes(csvInfo.Size()),
		"compressed_size", formatBytes(gzInfo.Size()),
		"ratio", fmt.Sprintf("%.1f%%", float64(gzInfo.Size())/float64(csvInfo.Size())*100))

	// Create stable filenames (without date) for deployment
	stableCsvPath := filepath.Join(outputDir, "vehicle-tracking.csv")
	stableGzPath := filepath.Join(outputDir, "vehicle-tracking.csv.gz")
	if err := copyFile(csvPath, stableCsvPath); err != nil {
		return fmt.Errorf("copy to stable csv: %w", err)
	}
	if err := copyFile(gzPath, stableGzPath); err != nil {
		return fmt.Errorf("copy to stable gz: %w", err)
	}
	logger.Info("wrote stable files", "csv", stableCsvPath, "gz", stableGzPath)

	return nil
}

func exportHistoricData(logger *slog.Logger, inputPath, outputDir, timestamp string) error {
	input, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer input.Close()

	// Historic file is already CSV, just copy and gzip
	csvName := fmt.Sprintf("historic-stop-observations-%s.csv", timestamp)
	csvPath := filepath.Join(outputDir, csvName)

	csvFile, err := os.Create(csvPath)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}

	lineCount, err := copyAndCount(input, csvFile)
	csvFile.Close()
	if err != nil {
		return fmt.Errorf("copy historic: %w", err)
	}

	logger.Info("wrote historic CSV", "file", csvPath, "lines", lineCount)

	// Create gzipped version
	gzPath := csvPath + ".gz"
	if err := gzipFile(csvPath, gzPath); err != nil {
		return fmt.Errorf("gzip historic: %w", err)
	}

	gzInfo, _ := os.Stat(gzPath)
	csvInfo, _ := os.Stat(csvPath)
	logger.Info("wrote gzipped historic CSV",
		"file", gzPath,
		"original_size", formatBytes(csvInfo.Size()),
		"compressed_size", formatBytes(gzInfo.Size()),
		"ratio", fmt.Sprintf("%.1f%%", float64(gzInfo.Size())/float64(csvInfo.Size())*100))

	// Create stable filenames (without date) for deployment
	stableCsvPath := filepath.Join(outputDir, "historic-stop-observations.csv")
	stableGzPath := filepath.Join(outputDir, "historic-stop-observations.csv.gz")
	if err := copyFile(csvPath, stableCsvPath); err != nil {
		return fmt.Errorf("copy to stable csv: %w", err)
	}
	if err := copyFile(gzPath, stableGzPath); err != nil {
		return fmt.Errorf("copy to stable gz: %w", err)
	}
	logger.Info("wrote stable files", "csv", stableCsvPath, "gz", stableGzPath)

	return nil
}

func copyAndCount(src io.Reader, dst io.Writer) (int, error) {
	scanner := bufio.NewScanner(src)
	// Increase buffer size for large lines
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, len(buf))

	writer := bufio.NewWriter(dst)
	count := 0
	for scanner.Scan() {
		writer.WriteString(scanner.Text())
		writer.WriteString("\n")
		count++
	}
	writer.Flush()
	return count, scanner.Err()
}

func gzipFile(src, dst string) error {
	input, err := os.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer output.Close()

	gzWriter := gzip.NewWriter(output)
	defer gzWriter.Close()

	_, err = io.Copy(gzWriter, input)
	return err
}

func copyFile(src, dst string) error {
	input, err := os.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer output.Close()

	_, err = io.Copy(output, input)
	return err
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
