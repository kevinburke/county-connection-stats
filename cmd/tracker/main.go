// tracker monitors real-time vehicle positions from the 511.org API.
//
// It continuously polls the VehicleMonitoring API to track buses in real-time,
// filtering by route number and logging positions to a TSV file for analysis.
// The program decouples API polling from data processing to avoid missing data
// during processing.
//
// Usage:
//
//	export API_511_KEY=your_key_here
//	go run ./cmd/tracker
//
// Options:
//
//	-route               Route number(s) to track, comma-separated (default: 4)
//	-api-poll-interval   How often to fetch from API (default: 1m, max 60/hour)
//	-process-interval    How often to process/write data (default: 1m)
//	-output              TSV file for vehicle observations
//	-save-response       Save raw HTTP response to file (for debugging)
//	-load-response       Load response from file instead of API (for testing)
package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/rest/restclient"
)

const (
	// 511.org API endpoint for vehicle positions
	apiURL = "https://api.511.org/transit/VehicleMonitoring"

	// County Connection agency code
	agencyCode = "CC"
)

type VehicleInfo struct {
	VehicleID string
	TripID    string
	RouteID   string
	Latitude  float32
	Longitude float32
	Bearing   float32
	Speed     float32
	Timestamp time.Time
}

// VehicleStore holds the latest vehicle data with thread-safe access
type VehicleStore struct {
	mu       sync.RWMutex
	vehicles []VehicleInfo
	lastSync time.Time
}

// SIRI JSON response structures
type SiriResponse struct {
	Siri struct {
		ServiceDelivery struct {
			ResponseTimestamp         string `json:"ResponseTimestamp"`
			ProducerRef               string `json:"ProducerRef"`
			VehicleMonitoringDelivery struct {
				Version           string            `json:"version"`
				ResponseTimestamp string            `json:"ResponseTimestamp"`
				VehicleActivity   []VehicleActivity `json:"VehicleActivity"`
			} `json:"VehicleMonitoringDelivery"`
		} `json:"ServiceDelivery"`
	} `json:"Siri"`
}

type VehicleActivity struct {
	RecordedAtTime          string `json:"RecordedAtTime"`
	MonitoredVehicleJourney struct {
		LineRef         *string `json:"LineRef"`
		DirectionRef    *string `json:"DirectionRef"`
		VehicleRef      string  `json:"VehicleRef"`
		VehicleLocation struct {
			Longitude string `json:"Longitude"`
			Latitude  string `json:"Latitude"`
		} `json:"VehicleLocation"`
		Bearing                 *string `json:"Bearing"`
		FramedVehicleJourneyRef struct {
			DataFrameRef           string  `json:"DataFrameRef"`
			DatedVehicleJourneyRef *string `json:"DatedVehicleJourneyRef"`
		} `json:"FramedVehicleJourneyRef"`
	} `json:"MonitoredVehicleJourney"`
}

func main() {
	saveResponse := flag.String("save-response", "", "Save raw HTTP response to file")
	loadResponse := flag.String("load-response", "", "Load response from file instead of making API call")
	apiPollInterval := flag.Duration("api-poll-interval", 1*time.Minute, "API polling interval (max 60 req/hour)")
	processInterval := flag.Duration("process-interval", 1*time.Minute, "Processing interval for writing data")
	routeFilter := flag.String("route", "4", "Filter vehicles by route number(s), comma-separated (e.g., '4' or '4,5')")
	outputFile := flag.String("output", "vehicle-tracking.tsv", "TSV file for vehicle observations")
	flag.Parse()

	// Parse routes into a slice
	routes := strings.Split(*routeFilter, ",")

	// Validate intervals
	if *processInterval < *apiPollInterval {
		slog.Error("process-interval must be >= api-poll-interval")
		os.Exit(1)
	}

	// Get API key from environment variable (only needed if not loading from file)
	apiKey := os.Getenv("API_511_KEY")
	if *loadResponse == "" && apiKey == "" {
		slog.Error("API_511_KEY environment variable not set. Get your free API key at: https://511.org/open-data/token")
		os.Exit(1)
	}

	// Open TSV file for writing
	tsvFile, err := openTSVFile(*outputFile)
	if err != nil {
		slog.Error("failed to open TSV file", "error", err)
		os.Exit(1)
	}
	defer tsvFile.Close()

	// Create shared vehicle store
	store := &VehicleStore{}

	// Start API polling goroutine
	go apiPoller(apiKey, *loadResponse, *saveResponse, *apiPollInterval, store)

	// Process updates in main goroutine
	processor(routes, *processInterval, store, tsvFile)
}

func (s *VehicleStore) Update(vehicles []VehicleInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vehicles = vehicles
	s.lastSync = time.Now()
}

func (s *VehicleStore) Get() ([]VehicleInfo, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy to avoid race conditions
	vehiclesCopy := make([]VehicleInfo, len(s.vehicles))
	copy(vehiclesCopy, s.vehicles)
	return vehiclesCopy, s.lastSync
}

func fetchRawResponse(apiKey string) ([]byte, error) {
	// Build request URL
	url := fmt.Sprintf("%s?api_key=%s&agency=%s&format=gtfsrt",
		apiURL, apiKey, agencyCode)

	slog.Info("fetching from API", "url", strings.Replace(url, apiKey, "***", 1))

	// Create HTTP client with debug transport
	client := &http.Client{
		Transport: restclient.DefaultTransport,
	}

	// Make HTTP request
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	slog.Info("received response",
		"status", resp.StatusCode,
		"content_type", resp.Header.Get("Content-Type"),
		"content_length", resp.Header.Get("Content-Length"))

	// Handle rate limiting
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := resp.Header.Get("Retry-After")
		body, _ := io.ReadAll(resp.Body)
		if retryAfter != "" {
			return nil, fmt.Errorf("rate limited (429): retry after %s seconds. Response: %s", retryAfter, string(body))
		}
		return nil, fmt.Errorf("rate limited (429): %s", string(body))
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Read response body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	slog.Debug("received data", "bytes", len(data))

	// Show first 200 bytes as debug info
	preview := data
	if len(preview) > 200 {
		preview = preview[:200]
	}
	slog.Debug("response preview", "preview", string(preview))

	return data, nil
}

// apiPoller fetches vehicle data at regular intervals and updates the store
func apiPoller(apiKey, loadFile, saveFile string, interval time.Duration, store *VehicleStore) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Do initial fetch immediately
	fetch := func() {
		var data []byte
		var err error

		if loadFile != "" {
			// Load from file (one-time only for replay)
			slog.Info("loading response from file", "file", loadFile)
			data, err = os.ReadFile(loadFile)
			if err != nil {
				slog.Error("failed to read response file", "error", err)
				return
			}
		} else {
			// Fetch from API
			data, err = fetchRawResponse(apiKey)
			if err != nil {
				slog.Error("failed to fetch vehicle positions", "error", err)
				return
			}

			// Save if requested
			if saveFile != "" {
				if err := os.WriteFile(saveFile, data, 0644); err != nil {
					slog.Error("failed to save response", "error", err)
				} else {
					slog.Info("saved response", "file", saveFile, "bytes", len(data))
				}
			}
		}

		// Parse the data
		vehicles, err := parseVehiclePositions(data)
		if err != nil {
			slog.Error("failed to parse vehicle positions", "error", err)
			return
		}

		// Update store
		store.Update(vehicles)
		slog.Info("updated vehicle store", "vehicles", len(vehicles))

		// If loading from file, we only do this once
		if loadFile != "" {
			ticker.Stop()
		}
	}

	// Initial fetch
	fetch()

	// Continue polling if not loading from file
	if loadFile == "" {
		for range ticker.C {
			fetch()
		}
	}
}

// openTSVFile opens a TSV file for appending, creating it with headers if it doesn't exist
func openTSVFile(filename string) (*os.File, error) {
	// Check if file exists
	_, err := os.Stat(filename)
	fileExists := err == nil

	// Open file for append/create
	file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	// Write header if file is new
	if !fileExists {
		writer := csv.NewWriter(file)
		writer.Comma = '\t'
		header := []string{
			"timestamp",
			"date",
			"time",
			"vehicle_id",
			"route",
			"trip_id",
			"latitude",
			"longitude",
			"bearing",
			"speed_mps",
		}
		if err := writer.Write(header); err != nil {
			file.Close()
			return nil, err
		}
		writer.Flush()
		slog.Info("created new TSV file", "file", filename)
	} else {
		slog.Info("appending to existing TSV file", "file", filename)
	}

	return file, nil
}

// processor filters and displays vehicle updates at a regular interval
func processor(routeFilters []string, interval time.Duration, store *VehicleStore, tsvFile *os.File) {
	writer := csv.NewWriter(tsvFile)
	writer.Comma = '\t'

	// Wait for initial data
	slog.Info("waiting for initial data")
	for {
		vehicles, _ := store.Get()
		if len(vehicles) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	process := func() {
		now := time.Now()

		// Get current vehicles from store
		vehicles, lastSync := store.Get()

		if len(vehicles) == 0 {
			slog.Warn("no vehicle data available")
			return
		}

		// Filter vehicles by route
		filtered := make([]VehicleInfo, 0)
		for _, v := range vehicles {
			// Check if vehicle's route is in the list of routes to monitor
			for _, route := range routeFilters {
				if v.RouteID == route {
					filtered = append(filtered, v)
					break
				}
			}
		}

		// Display results
		routesStr := strings.Join(routeFilters, ", ")
		fmt.Printf("\n\nRoute %s Vehicle Positions (%s)\n", routesStr, now.Format("2006-01-02 15:04:05"))
		fmt.Printf("Data last synced: %s\n", lastSync.Format("2006-01-02 15:04:05"))
		fmt.Println(strings.Repeat("=", 80))

		if len(filtered) == 0 {
			fmt.Printf("No active vehicles found on route(s) %s.\n", routesStr)
			return
		}

		for _, v := range filtered {
			// Display to console
			fmt.Printf("\nVehicle ID:  %s\n", v.VehicleID)
			fmt.Printf("Route:       %s\n", v.RouteID)
			fmt.Printf("Trip ID:     %s\n", v.TripID)
			fmt.Printf("Position:    %.6f, %.6f\n", v.Latitude, v.Longitude)
			if v.Speed > 0 {
				fmt.Printf("Speed:       %.1f m/s (%.1f mph)\n", v.Speed, v.Speed*2.237)
			}
			if v.Bearing > 0 {
				fmt.Printf("Bearing:     %.1f°\n", v.Bearing)
			}
			fmt.Printf("Updated:     %s\n", v.Timestamp.Format("15:04:05"))
			fmt.Println(strings.Repeat("-", 80))

			// Write to TSV
			record := []string{
				now.Format(time.RFC3339),
				now.Format("2006-01-02"),
				now.Format("15:04:05"),
				v.VehicleID,
				v.RouteID,
				v.TripID,
				fmt.Sprintf("%.6f", v.Latitude),
				fmt.Sprintf("%.6f", v.Longitude),
				fmt.Sprintf("%.1f", v.Bearing),
				fmt.Sprintf("%.2f", v.Speed),
			}
			if err := writer.Write(record); err != nil {
				slog.Error("failed to write to TSV", "error", err)
			}
		}

		writer.Flush()
		if err := writer.Error(); err != nil {
			slog.Error("failed to flush TSV writer", "error", err)
		}

		fmt.Printf("\nTotal route %s vehicles: %d (of %d total)\n", routesStr, len(filtered), len(vehicles))
		fmt.Printf("Data appended to %s\n", tsvFile.Name())
	}

	// Initial process
	process()

	// Continue processing at interval
	for range ticker.C {
		process()
	}
}

func parseVehiclePositions(data []byte) ([]VehicleInfo, error) {
	// Strip UTF-8 BOM if present
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	// Parse SIRI JSON
	var siri SiriResponse
	if err := json.Unmarshal(data, &siri); err != nil {
		return nil, fmt.Errorf("failed to parse SIRI JSON data: %w", err)
	}

	// Extract vehicle information
	var vehicles []VehicleInfo

	for _, activity := range siri.Siri.ServiceDelivery.VehicleMonitoringDelivery.VehicleActivity {
		journey := activity.MonitoredVehicleJourney
		info := VehicleInfo{}

		// Vehicle ID
		info.VehicleID = journey.VehicleRef

		// Route ID
		if journey.LineRef != nil {
			info.RouteID = *journey.LineRef
		}

		// Trip ID
		if journey.FramedVehicleJourneyRef.DatedVehicleJourneyRef != nil {
			info.TripID = *journey.FramedVehicleJourneyRef.DatedVehicleJourneyRef
		}

		// Latitude
		if lat, err := strconv.ParseFloat(journey.VehicleLocation.Latitude, 32); err == nil {
			info.Latitude = float32(lat)
		}

		// Longitude
		if lon, err := strconv.ParseFloat(journey.VehicleLocation.Longitude, 32); err == nil {
			info.Longitude = float32(lon)
		}

		// Bearing
		if journey.Bearing != nil {
			if bearing, err := strconv.ParseFloat(*journey.Bearing, 32); err == nil {
				info.Bearing = float32(bearing)
			}
		}

		// Timestamp
		if t, err := time.Parse(time.RFC3339, activity.RecordedAtTime); err == nil {
			info.Timestamp = t
		}

		vehicles = append(vehicles, info)
	}

	return vehicles, nil
}
