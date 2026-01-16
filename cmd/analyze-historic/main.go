// analyze-historic analyzes historic GTFS data to find vehicles operating specific routes.
//
// It reads the historic GTFS data (routes.txt, trips.txt, stop_observations.txt)
// and identifies which vehicles operated on a given route, along with trip counts
// and observation statistics.
//
// Usage:
//
//	# Analyze route 4 across all downloaded months
//	go run ./cmd/analyze-historic
//
//	# Analyze with agency prefix (for routes like "CC:4")
//	go run ./cmd/analyze-historic -route 4 -agency CC
//
//	# Analyze a specific month
//	go run ./cmd/analyze-historic -month 2024-01
//
//	# Output as TSV for further processing
//	go run ./cmd/analyze-historic -format tsv > route4-vehicles.tsv
//
// Options:
//
//	-data      Directory containing historic data (default: var/historic/county-connection)
//	-route     Route ID to analyze (default: 4)
//	-agency    Agency prefix for route IDs (e.g., "CC" for "CC:4")
//	-format    Output format: summary, detailed, tsv (default: summary)
//	-month     Analyze specific month (YYYY-MM), or all if not specified
//	-version   Print version and exit
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const (
	analyzeVersion        = "0.1.0"
	defaultDataDir        = "var/historic/county-connection"
	defaultRouteID        = "4"
	qualityInvalidTripMax = 10
	qualityMinTotalTrips  = 200
	qualityMinValidTrips  = 200
)

var blockVehiclePattern = regexp.MustCompile(`^block_\d+_schedBasedVehicle$`)

var (
	versionFlag   = flag.Bool("version", false, "Print version and exit")
	dataDir       = flag.String("data", defaultDataDir, "Directory containing historic GTFS data")
	routeID       = flag.String("route", defaultRouteID, "Route ID to analyze")
	agency        = flag.String("agency", "", "Agency prefix for route IDs (e.g., 'CC' for 'CC:4')")
	outputFormat  = flag.String("format", "summary", "Output format: summary, detailed, tsv")
	specificMonth = flag.String("month", "", "Analyze specific month (YYYY-MM), or all months if not specified")
	parallelism   = flag.Int("parallel", runtime.NumCPU(), "Number of months to process concurrently")
	dropBadMonths = flag.Bool("drop-bad-months", false, "Skip months failing the quality heuristics (<=10 invalid trips, >=200 total trips, >=200 valid trips) when producing non-quality output")
)

type Route struct {
	RouteID   string
	ShortName string
	LongName  string
}

type Trip struct {
	TripID    string
	RouteID   string
	ServiceID string
}

type StopObservation struct {
	TripID                string
	StopSequence          string
	StopID                string
	VehicleID             string
	ObservedArrivalTime   string
	ObservedDepartureTime string
}

type VehicleStats struct {
	VehicleID        string
	TripCount        int
	ObservationCount int
	FirstSeen        string
	LastSeen         string
}

type TripStats struct {
	Total   int
	Valid   int
	Invalid int
	Unknown int
}

type tripQuality int

const (
	tripUnknown tripQuality = iota
	tripValid
	tripInvalid
)

type MonthResult struct {
	Month        string
	Vehicles     map[string]*VehicleStats
	TripStats    TripStats
	Observations int
}

type monthTaskResult struct {
	MonthResult
	err error
}

func main() {
	flag.Parse()

	targetRoutes := parseRouteList(*routeID)
	routeLabel := strings.Join(targetRoutes, ",")

	if *versionFlag {
		fmt.Printf("analyze-historic version %s\n", analyzeVersion)
		os.Exit(0)
	}

	// Check data directory exists
	if _, err := os.Stat(*dataDir); os.IsNotExist(err) {
		slog.Error("Data directory does not exist", "dir", *dataDir)
		slog.Info("Run download-historic first to download the data")
		os.Exit(1)
	}

	// Get list of months to process
	months, err := getMonthsToProcess(*dataDir, *specificMonth)
	if err != nil {
		slog.Error("Failed to get months", "error", err)
		os.Exit(1)
	}

	if len(months) == 0 {
		slog.Error("No data found", "dir", *dataDir)
		os.Exit(1)
	}

	slog.Info("Analyzing historic data",
		"routes", routeLabel,
		"months", len(months),
		"format", *outputFormat)

	// Process months concurrently
	allVehicles := make(map[string]*VehicleStats)
	totalTrips := 0
	totalObservations := 0
	var monthResults []MonthResult

	parallel := *parallelism
	if parallel < 1 {
		parallel = 1
	}

	resultsCh := make(chan monthTaskResult)
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for _, month := range months {
		month := month
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			monthDir := filepath.Join(*dataDir, month)
			vehicles, tripStats, observations, err := processMonth(monthDir, targetRoutes, *agency)
			if err != nil {
				resultsCh <- monthTaskResult{
					MonthResult: MonthResult{Month: month},
					err:         err,
				}
				return
			}

			resultsCh <- monthTaskResult{
				MonthResult: MonthResult{
					Month:        month,
					Vehicles:     vehicles,
					TripStats:    tripStats,
					Observations: observations,
				},
			}
		}()
	}

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	for res := range resultsCh {
		if res.err != nil {
			slog.Error("Failed to process month", "month", res.Month, "error", res.err)
			continue
		}

		monthResults = append(monthResults, res.MonthResult)

		if *specificMonth != "" {
			slog.Info("Month processed",
				"month", res.Month,
				"vehicles", len(res.Vehicles),
				"trips_total", res.TripStats.Total,
				"trips_valid", res.TripStats.Valid,
				"trips_invalid", res.TripStats.Invalid,
				"observations", res.Observations)
		}
	}

	sort.Slice(monthResults, func(i, j int) bool {
		return monthResults[i].Month < monthResults[j].Month
	})

	dropMonths := *dropBadMonths && *outputFormat != "quality"
	if dropMonths {
		var filtered []MonthResult
		for _, result := range monthResults {
			if passes, reason := monthPassesQuality(result); passes {
				filtered = append(filtered, result)
			} else {
				slog.Warn("Dropping month due to quality filters", "month", result.Month, "reason", reason)
			}
		}
		if len(filtered) == 0 {
			slog.Error("All months dropped by quality filters", "invalid_trip_max", qualityInvalidTripMax, "min_total_trips", qualityMinTotalTrips, "min_valid_trips", qualityMinValidTrips)
			os.Exit(1)
		}
		monthResults = filtered
	}

	for _, result := range monthResults {
		for vehicleID, stats := range result.Vehicles {
			if existing, ok := allVehicles[vehicleID]; ok {
				existing.TripCount += stats.TripCount
				existing.ObservationCount += stats.ObservationCount
				if stats.FirstSeen < existing.FirstSeen {
					existing.FirstSeen = stats.FirstSeen
				}
				if stats.LastSeen > existing.LastSeen {
					existing.LastSeen = stats.LastSeen
				}
			} else {
				allVehicles[vehicleID] = stats
			}
		}

		totalTrips += result.TripStats.Total
		totalObservations += result.Observations
	}

	// Output results
	if err := outputResults(allVehicles, totalTrips, totalObservations, routeLabel, *outputFormat, monthResults); err != nil {
		slog.Error("Failed to output results", "error", err)
		os.Exit(1)
	}
}

// getMonthsToProcess returns a list of month directories to process
func getMonthsToProcess(dataDir, specificMonth string) ([]string, error) {
	if specificMonth != "" {
		// Check if specific month exists
		monthPath := filepath.Join(dataDir, specificMonth)
		if _, err := os.Stat(monthPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("month %s not found", specificMonth)
		}
		return []string{specificMonth}, nil
	}

	// Get all month directories
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}

	var months []string
	for _, entry := range entries {
		if entry.IsDir() && len(entry.Name()) == 7 && entry.Name()[4] == '-' {
			// Looks like YYYY-MM format
			months = append(months, entry.Name())
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
	if len(routes) == 0 {
		return []string{defaultRouteID}
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

// processMonth analyzes a single month of historic data
func processMonth(monthDir string, targetRoutes []string, agency string) (map[string]*VehicleStats, TripStats, int, error) {
	// Read routes.txt to verify route exists
	routes, err := readRoutes(filepath.Join(monthDir, "routes.txt"))
	if err != nil {
		return nil, TripStats{}, 0, fmt.Errorf("failed to read routes: %w", err)
	}

	var routeFound bool
	for _, route := range routes {
		if routeMatches(route.RouteID, targetRoutes, agency) || routeMatches(route.ShortName, targetRoutes, agency) {
			routeFound = true
			break
		}
	}

	if !routeFound {
		return nil, TripStats{}, 0, fmt.Errorf("routes %v not found in routes.txt", targetRoutes)
	}

	// Read trips.txt to find trips for this route
	trips, err := readTrips(filepath.Join(monthDir, "trips.txt"))
	if err != nil {
		return nil, TripStats{}, 0, fmt.Errorf("failed to read trips: %w", err)
	}

	// Build set of trip IDs for this route
	routeTripIDs := make(map[string]bool)
	for _, trip := range trips {
		if routeMatches(trip.RouteID, targetRoutes, agency) {
			routeTripIDs[trip.TripID] = true
		}
	}

	if len(routeTripIDs) == 0 {
		return nil, TripStats{}, 0, fmt.Errorf("no trips found for routes %v", targetRoutes)
	}

	// Stream stop_observations.txt to collect vehicle statistics
	// This avoids loading the entire file (potentially GB) into memory
	vehicles, tripStats, observationCount, err := streamStopObservations(
		filepath.Join(monthDir, "stop_observations.txt"),
		routeTripIDs,
	)
	if err != nil {
		return nil, TripStats{}, 0, fmt.Errorf("failed to process stop_observations: %w", err)
	}

	return vehicles, tripStats, observationCount, nil
}

// streamStopObservations processes stop_observations.txt using a streaming approach
func streamStopObservations(path string, routeTripIDs map[string]bool) (map[string]*VehicleStats, TripStats, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, TripStats{}, 0, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, TripStats{}, 0, err
	}

	colIndex := make(map[string]int)
	for i, col := range header {
		colIndex[col] = i
	}

	tripIdx, ok := colIndex["trip_id"]
	if !ok {
		return nil, TripStats{}, 0, fmt.Errorf("trip_id column missing in %s", path)
	}

	vehicleIdx := -1
	if idx, ok := colIndex["vehicle_id"]; ok {
		vehicleIdx = idx
	}

	arrivalIdx := -1
	if idx, ok := colIndex["observed_arrival_time"]; ok {
		arrivalIdx = idx
	}

	serviceIdx := -1
	if idx, ok := colIndex["service_date"]; ok {
		serviceIdx = idx
	}

	vehicles := make(map[string]*VehicleStats)
	tripsByVehicle := make(map[string]map[string]struct{})
	observationCount := 0
	tripStats := TripStats{}
	tripStatus := make(map[string]tripQuality)

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Skip malformed lines
			continue
		}

		tripID := valueAt(record, tripIdx)
		if !routeTripIDs[tripID] {
			continue
		}

		serviceDate := valueAt(record, serviceIdx)
		tripKey := makeTripKey(tripID, serviceDate)
		if tripKey == "" {
			continue
		}

		recordTripQuality(tripKey, tripUnknown, &tripStats, tripStatus)

		observationCount++

		vehicleID := valueAt(record, vehicleIdx)
		if vehicleID == "" {
			continue
		}
		if shouldIgnoreVehicle(vehicleID) {
			continue
		}

		quality := tripValid
		if !isVehicleIDValid(vehicleID) {
			quality = tripInvalid
		}
		recordTripQuality(tripKey, quality, &tripStats, tripStatus)

		arrival := valueAt(record, arrivalIdx)

		stats, ok := vehicles[vehicleID]
		if !ok {
			stats = &VehicleStats{
				VehicleID: vehicleID,
				FirstSeen: arrival,
				LastSeen:  arrival,
			}
			vehicles[vehicleID] = stats
		}

		stats.ObservationCount++

		if arrival != "" {
			if stats.FirstSeen == "" || arrival < stats.FirstSeen {
				stats.FirstSeen = arrival
			}
			if stats.LastSeen == "" || arrival > stats.LastSeen {
				stats.LastSeen = arrival
			}
		}

		tripsForVehicle := tripsByVehicle[vehicleID]
		if tripsForVehicle == nil {
			tripsForVehicle = make(map[string]struct{})
			tripsByVehicle[vehicleID] = tripsForVehicle
		}
		if _, seen := tripsForVehicle[tripKey]; !seen {
			tripsForVehicle[tripKey] = struct{}{}
			stats.TripCount++
		}
	}

	return vehicles, tripStats, observationCount, nil
}

// readRoutes reads the routes.txt file
func readRoutes(path string) ([]Route, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	// Read header
	header, err := reader.Read()
	if err != nil {
		return nil, err
	}

	// Find column indices
	colIndex := make(map[string]int)
	for i, col := range header {
		colIndex[col] = i
	}

	// Read routes
	var routes []Route
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		route := Route{}
		if idx, ok := colIndex["route_id"]; ok && idx < len(record) {
			route.RouteID = record[idx]
		}
		if idx, ok := colIndex["route_short_name"]; ok && idx < len(record) {
			route.ShortName = record[idx]
		}
		if idx, ok := colIndex["route_long_name"]; ok && idx < len(record) {
			route.LongName = record[idx]
		}

		routes = append(routes, route)
	}

	return routes, nil
}

// readTrips reads the trips.txt file
func readTrips(path string) ([]Trip, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	// Read header
	header, err := reader.Read()
	if err != nil {
		return nil, err
	}

	// Find column indices
	colIndex := make(map[string]int)
	for i, col := range header {
		colIndex[col] = i
	}

	// Read trips
	var trips []Trip
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		trip := Trip{}
		if idx, ok := colIndex["trip_id"]; ok && idx < len(record) {
			trip.TripID = record[idx]
		}
		if idx, ok := colIndex["route_id"]; ok && idx < len(record) {
			trip.RouteID = record[idx]
		}
		if idx, ok := colIndex["service_id"]; ok && idx < len(record) {
			trip.ServiceID = record[idx]
		}

		trips = append(trips, trip)
	}

	return trips, nil
}

// readStopObservations reads the stop_observations.txt file
func readStopObservations(path string) ([]StopObservation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	// Read header
	header, err := reader.Read()
	if err != nil {
		return nil, err
	}

	// Find column indices
	colIndex := make(map[string]int)
	for i, col := range header {
		colIndex[col] = i
	}

	// Read observations
	var observations []StopObservation
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Skip malformed lines
			continue
		}

		obs := StopObservation{}
		if idx, ok := colIndex["trip_id"]; ok && idx < len(record) {
			obs.TripID = record[idx]
		}
		if idx, ok := colIndex["stop_sequence"]; ok && idx < len(record) {
			obs.StopSequence = record[idx]
		}
		if idx, ok := colIndex["stop_id"]; ok && idx < len(record) {
			obs.StopID = record[idx]
		}
		if idx, ok := colIndex["vehicle_id"]; ok && idx < len(record) {
			obs.VehicleID = record[idx]
		}
		if idx, ok := colIndex["observed_arrival_time"]; ok && idx < len(record) {
			obs.ObservedArrivalTime = record[idx]
		}
		if idx, ok := colIndex["observed_departure_time"]; ok && idx < len(record) {
			obs.ObservedDepartureTime = record[idx]
		}

		observations = append(observations, obs)
	}

	return observations, nil
}

func valueAt(record []string, idx int) string {
	if idx >= 0 && idx < len(record) {
		return record[idx]
	}
	return ""
}

func makeTripKey(tripID, serviceDate string) string {
	if tripID == "" {
		return ""
	}
	if serviceDate == "" {
		return tripID
	}
	return tripID + "|" + serviceDate
}

func recordTripQuality(tripKey string, quality tripQuality, stats *TripStats, status map[string]tripQuality) {
	if tripKey == "" {
		return
	}

	prev, exists := status[tripKey]
	if !exists {
		status[tripKey] = tripUnknown
		stats.Total++
		stats.Unknown++
		prev = tripUnknown
	}

	if quality == tripUnknown || quality == prev {
		return
	}

	switch prev {
	case tripUnknown:
		stats.Unknown--
	case tripValid:
		stats.Valid--
	case tripInvalid:
		stats.Invalid--
	}

	status[tripKey] = quality
	switch quality {
	case tripUnknown:
		stats.Unknown++
	case tripValid:
		stats.Valid++
	case tripInvalid:
		stats.Invalid++
	}
}

// outputResults outputs the analysis results in the specified format
func outputResults(vehicles map[string]*VehicleStats, totalTrips, totalObservations int, routeLabel, format string, monthResults []MonthResult) error {
	// Sort vehicles by ID
	vehicleIDs := make([]string, 0, len(vehicles))
	for id := range vehicles {
		vehicleIDs = append(vehicleIDs, id)
	}
	sort.Strings(vehicleIDs)

	switch format {
	case "summary":
		fmt.Printf("\n=== Route %s Vehicle Analysis ===\n\n", routeLabel)
		fmt.Printf("Total vehicles found: %d\n", len(vehicles))
		fmt.Printf("Total trips: %d\n", totalTrips)
		fmt.Printf("Total observations: %d\n\n", totalObservations)

		if len(vehicles) > 0 {
			fmt.Println("Vehicles:")
			for _, id := range vehicleIDs {
				stats := vehicles[id]
				fmt.Printf("  %s: %d trips, %d observations\n",
					stats.VehicleID, stats.TripCount, stats.ObservationCount)
			}
		}

	case "detailed":
		fmt.Printf("\n=== Route %s Vehicle Analysis (Detailed) ===\n\n", routeLabel)
		fmt.Printf("Total vehicles: %d\n", len(vehicles))
		fmt.Printf("Total trips: %d\n", totalTrips)
		fmt.Printf("Total observations: %d\n\n", totalObservations)

		for _, id := range vehicleIDs {
			stats := vehicles[id]
			fmt.Printf("Vehicle: %s\n", stats.VehicleID)
			fmt.Printf("  Trips: %d\n", stats.TripCount)
			fmt.Printf("  Observations: %d\n", stats.ObservationCount)
			fmt.Printf("  First seen: %s\n", stats.FirstSeen)
			fmt.Printf("  Last seen: %s\n", stats.LastSeen)
			fmt.Println()
		}

	case "tsv":
		writer := csv.NewWriter(os.Stdout)
		writer.Comma = '\t'

		// Write header
		header := []string{"vehicle_id", "trip_count", "observation_count", "first_seen", "last_seen"}
		if err := writer.Write(header); err != nil {
			return err
		}

		// Write data
		for _, id := range vehicleIDs {
			stats := vehicles[id]
			record := []string{
				stats.VehicleID,
				fmt.Sprintf("%d", stats.TripCount),
				fmt.Sprintf("%d", stats.ObservationCount),
				stats.FirstSeen,
				stats.LastSeen,
			}
			if err := writer.Write(record); err != nil {
				return err
			}
		}

		writer.Flush()
		return writer.Error()

	case "quality":
		fmt.Printf("\n=== Vehicle ID Quality Report for Route %s ===\n\n", routeLabel)
		if len(monthResults) == 0 {
			fmt.Println("No months to report")
			return nil
		}

		var keepMonths []string
		var discardMonths []string

		for _, result := range monthResults {
			valid, invalid := splitVehiclesByQuality(result.Vehicles)
			validObs := sumObservationCount(valid)
			invalidObs := sumObservationCount(invalid)
			validTrips := result.TripStats.Valid
			invalidTrips := result.TripStats.Invalid

			fmt.Printf("%s: %d vehicles (%d valid, %d invalid) | trips valid=%d invalid=%d unknown=%d | observations valid=%d invalid=%d\n",
				result.Month, len(result.Vehicles), len(valid), len(invalid),
				validTrips, invalidTrips, result.TripStats.Unknown, validObs, invalidObs)

			if len(invalid) > 0 {
				fmt.Printf("  Invalid IDs: %s\n", formatVehicleList(invalid, 8))
			}
			if len(valid) > 0 {
				fmt.Printf("  Valid IDs:   %s\n", formatVehicleList(valid, 8))
			}
			fmt.Println()

			monthTripTotal := result.TripStats.Total
			entry := fmt.Sprintf("%s (%d invalid / %d total trips, %d valid trips)",
				result.Month, invalidTrips, monthTripTotal, validTrips)

			if passes, failReason := monthPassesQuality(result); passes {
				keepMonths = append(keepMonths, entry)
			} else {
				discardMonths = append(discardMonths, fmt.Sprintf("%s [%s]", entry, failReason))
			}
		}

		if len(keepMonths) > 0 {
			fmt.Printf("Months passing filters (<= %d invalid trips, >= %d total trips, >= %d valid trips) (%d):\n",
				qualityInvalidTripMax, qualityMinTotalTrips, qualityMinValidTrips, len(keepMonths))
			for _, entry := range keepMonths {
				fmt.Printf("  - %s\n", entry)
			}
			fmt.Println()
		}

		if len(discardMonths) > 0 {
			fmt.Printf("Months failing filters (%d):\n", len(discardMonths))
			for _, entry := range discardMonths {
				fmt.Printf("  - %s\n", entry)
			}
			fmt.Println()
		}

	default:
		return fmt.Errorf("unknown output format: %s", format)
	}

	return nil
}

func monthPassesQuality(result MonthResult) (bool, string) {
	switch {
	case result.TripStats.Total < qualityMinTotalTrips:
		return false, fmt.Sprintf("total trips < %d", qualityMinTotalTrips)
	case result.TripStats.Valid < qualityMinValidTrips:
		return false, fmt.Sprintf("valid trips < %d", qualityMinValidTrips)
	case result.TripStats.Invalid > qualityInvalidTripMax:
		return false, fmt.Sprintf("invalid trips > %d", qualityInvalidTripMax)
	default:
		return true, ""
	}
}

func splitVehiclesByQuality(vehicles map[string]*VehicleStats) (valid []*VehicleStats, invalid []*VehicleStats) {
	for _, stats := range vehicles {
		if isVehicleIDValid(stats.VehicleID) {
			valid = append(valid, stats)
		} else {
			invalid = append(invalid, stats)
		}
	}

	sort.Slice(valid, func(i, j int) bool {
		return valid[i].ObservationCount > valid[j].ObservationCount
	})
	sort.Slice(invalid, func(i, j int) bool {
		return invalid[i].ObservationCount > invalid[j].ObservationCount
	})

	return valid, invalid
}

func isVehicleIDValid(id string) bool {
	if id == "" {
		return false
	}
	if len(id) >= 6 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func sumObservationCount(stats []*VehicleStats) int {
	total := 0
	for _, s := range stats {
		total += s.ObservationCount
	}
	return total
}

func shouldIgnoreVehicle(id string) bool {
	return blockVehiclePattern.MatchString(id)
}

func formatVehicleList(stats []*VehicleStats, limit int) string {
	if len(stats) == 0 {
		return "(none)"
	}
	if len(stats) > limit {
		stats = stats[:limit]
	}

	entries := make([]string, 0, len(stats))
	for _, s := range stats {
		entries = append(entries, fmt.Sprintf("%s (%d trips, %d obs)", s.VehicleID, s.TripCount, s.ObservationCount))
	}
	result := strings.Join(entries, ", ")
	if len(stats) == limit {
		result += ", ..."
	}
	return result
}
