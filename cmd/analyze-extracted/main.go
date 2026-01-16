package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
)

const (
	deadheadMiles  = 8.7 * 2
	tripMiles      = 3.4
	minDieselTrips = 10
)

var (
	fileFlag   = flag.String("file", "var/extracted/cc45-stop-observations.txt", "Extracted stop_observations file to analyze")
	routesFlag = flag.String("routes", "4,5", "Comma-separated route IDs (for reporting label only)")
	agency     = flag.String("agency", "CC", "Agency prefix")
	showDiesel = flag.Bool("show-diesel", true, "Include per-diesel bus breakdown")
	group1600  = []string{"1600", "1601", "1602", "1603"}
	group1800  = []string{"1800", "1801", "1802", "1803"}
	bebSet     = map[string]struct{}{
		"1600": {}, "1601": {}, "1602": {}, "1603": {},
		"1800": {}, "1801": {}, "1802": {}, "1803": {},
	}
)

type BusStats struct {
	VehicleID   string
	Type        string
	TripCount   int
	DayTripMaps map[string]int // date -> trips
}

type BusResult struct {
	VehicleID    string
	Type         string
	TripCount    int
	ServiceDays  int
	Availability float64
	OutOfService int
	OutageRanges []OutageRange
	TotalMiles   float64
	FailureCount int
	MDBF         float64
	MilesBetween []float64
}

type OutageRange struct {
	Start string
	End   string
	Len   int
}

func main() {
	flag.Parse()

	file, err := os.Open(*fileFlag)
	if err != nil {
		slog.Error("Failed to open extracted file", "file", *fileFlag, "error", err)
		os.Exit(1)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		slog.Error("Failed to read header", "error", err)
		os.Exit(1)
	}

	cols := map[string]int{}
	for i, col := range header {
		cols[col] = i
	}

	required := []string{"trip_id", "service_date", "vehicle_id"}
	for _, c := range required {
		if _, ok := cols[c]; !ok {
			slog.Error("Missing column in extracted file", "column", c)
			os.Exit(1)
		}
	}

	tripVehicle := make(map[string]string)
	busStats := make(map[string]*BusStats)
	totalTripsByType := map[string]int{"BEB": 0, "diesel": 0}

	lines := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		lines++

		vehicleID := valueAt(record, cols["vehicle_id"])
		if !isValidVehicleID(vehicleID) {
			continue
		}

		serviceDate := valueAt(record, cols["service_date"])
		tripID := valueAt(record, cols["trip_id"])
		if serviceDate == "" || tripID == "" {
			continue
		}
		tripKey := serviceDate + "|" + tripID
		if _, exists := tripVehicle[tripKey]; exists {
			continue
		}
		tripVehicle[tripKey] = vehicleID

		busType := classifyBus(vehicleID)
		totalTripsByType[busType]++

		bs := busStats[vehicleID]
		if bs == nil {
			bs = &BusStats{
				VehicleID:   vehicleID,
				Type:        busType,
				DayTripMaps: make(map[string]int),
			}
			busStats[vehicleID] = bs
		}
		bs.TripCount++
		bs.DayTripMaps[serviceDate]++
	}

	slog.Info("File processed", "file", *fileFlag, "records", lines, "trips", len(tripVehicle))

	for id, stats := range busStats {
		if stats.Type == "diesel" && stats.TripCount < minDieselTrips {
			totalTripsByType["diesel"] -= stats.TripCount
			delete(busStats, id)
		}
	}

	validDateSet := make(map[string]struct{})
	for _, stats := range busStats {
		for date := range stats.DayTripMaps {
			validDateSet[date] = struct{}{}
		}
	}
	validDates := make([]string, 0, len(validDateSet))
	for date := range validDateSet {
		validDates = append(validDates, date)
	}
	sort.Strings(validDates)
	totalValidDays := len(validDates)
	if totalValidDays == 0 {
		slog.Error("No valid days detected")
		os.Exit(1)
	}

	busResults := make([]BusResult, 0, len(busStats))
	for _, stats := range busStats {
		result := computeBusResult(stats, validDates, totalValidDays)
		busResults = append(busResults, result)
	}
	sort.Slice(busResults, func(i, j int) bool {
		return busResults[i].VehicleID < busResults[j].VehicleID
	})

	fmt.Printf("\n=== Route %s Trip Availability ===\n", strings.Join(parseRouteList(*routesFlag), ","))
	fmt.Printf("Total valid days observed: %d\n", totalValidDays)
	fmt.Printf("Trips by BEB: %d\n", totalTripsByType["BEB"])
	fmt.Printf("Trips by diesel: %d\n\n", totalTripsByType["diesel"])

	fmt.Println("== Battery Electric Buses ==")
	for _, br := range busResults {
		if br.Type == "BEB" {
			printBusResult(br)
		}
	}

	group1600Result := computeGroupResult("Group 1600-1603", group1600, busStats, validDates, totalValidDays)
	group1800Result := computeGroupResult("Group 1800-1803", group1800, busStats, validDates, totalValidDays)

	fmt.Println("\n-- Group Summary --")
	printBusResult(group1600Result)
	printBusResult(group1800Result)

	if *showDiesel {
		fmt.Println("\n== Diesel Buses ==")
		for _, br := range busResults {
			if br.Type == "diesel" {
				printBusResult(br)
			}
		}
	}
}

func computeBusResult(stats *BusStats, validDates []string, totalValidDays int) BusResult {
	result := BusResult{
		VehicleID:    stats.VehicleID,
		Type:         stats.Type,
		TripCount:    stats.TripCount,
		OutageRanges: []OutageRange{},
	}

	serviceDays := len(stats.DayTripMaps)
	result.ServiceDays = serviceDays
	result.Availability = float64(serviceDays) / float64(totalValidDays)

	milesSinceFailure := 0.0
	outageStart := ""
	outageLength := 0

	for i, date := range validDates {
		trips := stats.DayTripMaps[date]
		if trips > 0 {
			miles := deadheadMiles + tripMiles*float64(trips)
			result.TotalMiles += miles
			milesSinceFailure += miles

			if outageLength >= 3 {
				result.FailureCount++
				if milesSinceFailure > 0 {
					result.MilesBetween = append(result.MilesBetween, milesSinceFailure)
				}
				endDate := validDates[i-1]
				result.OutageRanges = append(result.OutageRanges, OutageRange{Start: outageStart, End: endDate, Len: outageLength})
				milesSinceFailure = 0
			}
			outageStart = ""
			outageLength = 0
		} else {
			result.OutOfService++
			if outageStart == "" {
				outageStart = date
			}
			outageLength++
			milesSinceFailure += deadheadMiles
		}
	}

	if outageLength >= 3 {
		result.FailureCount++
		if milesSinceFailure > 0 {
			result.MilesBetween = append(result.MilesBetween, milesSinceFailure)
		}
		result.OutageRanges = append(result.OutageRanges, OutageRange{
			Start: outageStart,
			End:   validDates[len(validDates)-1],
			Len:   outageLength,
		})
	}

	if result.FailureCount > 0 {
		result.MDBF = result.TotalMiles / float64(result.FailureCount)
	}

	return result
}

func computeGroupResult(name string, members []string, busStats map[string]*BusStats, validDates []string, totalValidDays int) BusResult {
	group := &BusStats{
		VehicleID:   name,
		Type:        "group",
		DayTripMaps: make(map[string]int),
	}
	for _, member := range members {
		if stats, ok := busStats[member]; ok {
			group.TripCount += stats.TripCount
			for day, trips := range stats.DayTripMaps {
				group.DayTripMaps[day] += trips
			}
		}
	}
	return computeBusResult(group, validDates, totalValidDays)
}

func printBusResult(br BusResult) {
	if br.Type == "diesel" {
		fmt.Printf("Bus %s (diesel): trips=%d (availability not calculated for rotating diesel fleet)\n", br.VehicleID, br.TripCount)
		return
	}

	fmt.Printf("Bus %s (%s): trips=%d service_days=%d availability=%.2f%% oos_days=%d failures=%d total_miles=%.1f",
		br.VehicleID, br.Type, br.TripCount, br.ServiceDays, br.Availability*100, br.OutOfService, br.FailureCount, br.TotalMiles)
	if br.FailureCount > 0 {
		fmt.Printf(" MDBF=%.1f", br.MDBF)
	}
	fmt.Println()
	if len(br.OutageRanges) > 0 {
		fmt.Println("  Out-of-service ranges (>=3 valid days):")
		for _, r := range br.OutageRanges {
			fmt.Printf("    %s to %s (%d days)\n", r.Start, r.End, r.Len)
		}
	}
}

func classifyBus(vehicleID string) string {
	if _, ok := bebSet[vehicleID]; ok {
		return "BEB"
	}
	return "diesel"
}

func isValidVehicleID(id string) bool {
	if len(id) != 3 && len(id) != 4 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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

func valueAt(record []string, idx int) string {
	if idx >= 0 && idx < len(record) {
		return record[idx]
	}
	return ""
}

func formatMonth(date string) string {
	if len(date) != 8 {
		return date
	}
	return date[:4] + "-" + date[4:6]
}
