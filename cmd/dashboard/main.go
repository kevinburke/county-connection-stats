package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// Schedule constants from Seamless Bay Area analysis
	roundTripHeadway = 40 // minutes
	actualRouteTime  = 23 // minutes
	chargingDwell    = 17 // minutes per round trip, wasted by diesels

	// Distance constants from analyze-extracted
	deadheadMiles = 8.7 * 2
	tripMiles     = 3.4
)

var (
	trackingFile = flag.String("tracking-file", "vehicle-tracking.tsv", "Real-time vehicle tracking TSV file")
	historicFile = flag.String("historic-file", "var/extracted/cc45-stop-observations.txt", "Extracted stop_observations file")
	outputFile   = flag.String("output", "var/dashboard.html", "Output HTML file path")
	routesFlag   = flag.String("routes", "4,5", "Comma-separated route IDs to analyze")
	version      = flag.Bool("version", false, "Print version and exit")

	pacificTZ *time.Location

	bebSet = map[string]struct{}{
		"1600": {}, "1601": {}, "1602": {}, "1603": {},
		"1800": {}, "1801": {}, "1802": {}, "1803": {},
	}
	group1600 = []string{"1600", "1601", "1602", "1603"}
	group1800 = []string{"1800", "1801", "1802", "1803"}

	// Walnut Creek BART bus area polygon (lat, lon pairs)
	walnutCreekBARTPolygon = [][2]float64{
		{37.90702176601014, -122.06856813397798},
		{37.90631565035961, -122.06699383073966},
		{37.90442920597368, -122.06813046461296},
		{37.90528481242584, -122.06935855178641},
	}
	walnutCreekBARTCenter = [2]float64{37.905762, -122.068012}
)

// DashboardData holds all data for the HTML template
type DashboardData struct {
	GeneratedAt      time.Time
	Routes           string
	LiveBuses        []LiveBus
	LiveBEBCount     int
	LiveDieselCount  int
	WeekendSplit     BEBDieselSplit
	WeekdaySplit     BEBDieselSplit
	BEBStats         []BusResult
	Group1600Stats   BusResult
	Group1800Stats   BusResult
	WeekSplit        BEBDieselSplit
	MonthSplit       BEBDieselSplit
	YearSplit        BEBDieselSplit
	WastedTime       WastedTimeStats
	TotalValidDays   int
	DataStartDate    string
	DataEndDate      string
	LastWeekendBEB   string // Last date a BEB ran on weekend, empty if never
	LastWeekendBEBID string // Vehicle ID of the BEB

	// Fields for "BEB drought" display - shown when no BEB has run for 30+ days
	LastAnyBEB       string // Last date any BEB ran, empty if never
	LastAnyBEBID     string // Vehicle ID of the BEB
	DaysSinceLastBEB int    // Number of days since last BEB run
	ShowBEBDrought   bool   // True if 30+ days since last BEB run
}

// LiveBus represents a currently running bus
type LiveBus struct {
	VehicleID string
	Type      string
	Route     string
	LastSeen  time.Time
	Lat       float64
	Lon       float64
}

// BEBDieselSplit holds trip count breakdown
type BEBDieselSplit struct {
	BEBTrips    int
	DieselTrips int
	TotalTrips  int
	BEBPct      float64
	DieselPct   float64
}

// WastedTimeStats holds diesel wasted time calculations
type WastedTimeStats struct {
	DieselTripsWeek     int
	DieselTripsMonth    int
	DieselTripsYear     int
	WastedMinutesWeek   float64
	WastedMinutesMonth  float64
	WastedMinutesYear   float64
	WastedHoursWeek     float64
	WastedHoursMonth    float64
	WastedHoursYear     float64
	PotentialExtraTrips int
}

// BusStats holds per-bus statistics during processing
type BusStats struct {
	VehicleID   string
	Type        string
	TripCount   int
	DayTripMaps map[string]int // date -> trips
}

// BusResult holds computed results for a bus
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
}

// OutageRange represents a period of no service
type OutageRange struct {
	Start string
	End   string
	Len   int
}

func main() {
	flag.Parse()

	if *version {
		fmt.Println("dashboard v0.1.0")
		os.Exit(0)
	}

	// Initialize Pacific timezone
	var err error
	pacificTZ, err = time.LoadLocation("America/Los_Angeles")
	if err != nil {
		slog.Warn("Could not load Pacific timezone, using UTC", "error", err)
		pacificTZ = time.UTC
	}

	slog.Info("Generating dashboard", "tracking", *trackingFile, "historic", *historicFile, "output", *outputFile)

	data := DashboardData{
		GeneratedAt: time.Now().In(pacificTZ),
		Routes:      strings.ReplaceAll(*routesFlag, ",", ", "),
	}

	// Load live buses and recent trip data from tracking file
	liveBuses, realtimeTrips, err := loadTrackingData(*trackingFile)
	if err != nil {
		slog.Warn("Could not load tracking data", "error", err)
	} else {
		data.LiveBuses = liveBuses
		for _, bus := range liveBuses {
			if bus.Type == "BEB" {
				data.LiveBEBCount++
			} else {
				data.LiveDieselCount++
			}
		}
	}

	// Load and analyze historic data
	busStats, tripsByDate, validDates, err := loadHistoricData(*historicFile)
	if err != nil {
		slog.Error("Could not load historic data", "error", err)
		os.Exit(1)
	}

	// Merge realtime trips into tripsByDate
	for dateStr, trips := range realtimeTrips {
		tripsByDate[dateStr] = append(tripsByDate[dateStr], trips...)
		// Add to validDates if not already present
		found := false
		for _, d := range validDates {
			if d == dateStr {
				found = true
				break
			}
		}
		if !found {
			validDates = append(validDates, dateStr)
		}
	}
	sort.Strings(validDates)

	if len(validDates) > 0 {
		data.TotalValidDays = len(validDates)
		data.DataStartDate = formatDate(validDates[0])
		data.DataEndDate = formatDate(validDates[len(validDates)-1])
	}

	// Calculate BEB stats
	data.BEBStats = computeAllBEBStats(busStats, validDates)
	data.Group1600Stats = computeGroupResult("1600-1603", group1600, busStats, validDates)
	data.Group1800Stats = computeGroupResult("1800-1803", group1800, busStats, validDates)

	// Calculate weekend vs weekday splits
	data.WeekendSplit, data.WeekdaySplit = computeWeekendWeekdaySplit(tripsByDate)

	// Find last weekend BEB run
	data.LastWeekendBEB, data.LastWeekendBEBID = findLastWeekendBEB(tripsByDate)

	// Find last any BEB run and calculate if we should show drought message
	lastBEBDate, lastBEBVehicle := findLastAnyBEB(tripsByDate)
	if lastBEBDate != "" {
		data.LastAnyBEB = formatDateHuman(lastBEBDate)
		data.LastAnyBEBID = lastBEBVehicle
		lastBEBTime := serviceDateToTime(lastBEBDate)
		data.DaysSinceLastBEB = int(time.Since(lastBEBTime).Hours() / 24)
		data.ShowBEBDrought = data.DaysSinceLastBEB >= 30
	}

	// Calculate time period splits
	now := time.Now()
	data.WeekSplit = computeTimePeriodSplit(tripsByDate, now.AddDate(0, 0, -7), now)
	data.MonthSplit = computeTimePeriodSplit(tripsByDate, now.AddDate(0, -1, 0), now)
	data.YearSplit = computeTimePeriodSplit(tripsByDate, now.AddDate(-1, 0, 0), now)

	// Calculate wasted time
	data.WastedTime = computeWastedTime(tripsByDate, now)

	// Generate HTML
	if err := generateHTML(data, *outputFile); err != nil {
		slog.Error("Failed to generate HTML", "error", err)
		os.Exit(1)
	}

	slog.Info("Dashboard generated", "output", *outputFile)
}

// loadTrackingData loads both live buses (last 15 min) and all trip data from tracking file
func loadTrackingData(filename string) ([]LiveBus, map[string][]TripInfo, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	var buses []LiveBus
	seen := make(map[string]LiveBus)
	tripsByDate := make(map[string][]TripInfo)
	tripsSeen := make(map[string]struct{}) // track unique trips per day
	cutoff := time.Now().Add(-15 * time.Minute)

	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum == 1 {
			continue // skip header
		}

		line := scanner.Text()
		fields := strings.Split(line, "\t")
		if len(fields) < 10 {
			continue
		}

		// timestamp, date, time, vehicle_id, route, trip_id, lat, lon, bearing, speed
		ts, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}

		vehicleID := fields[3]
		if !isValidVehicleID(vehicleID) {
			continue
		}

		dateStr := fields[1] // YYYY-MM-DD format
		tripID := fields[5]
		busType := classifyBus(vehicleID)

		// Convert date to YYYYMMDD format for consistency with historic data
		dateKey := strings.ReplaceAll(dateStr, "-", "")

		// Track unique trips per day for time period calculations
		tripKey := dateKey + "|" + tripID
		if _, exists := tripsSeen[tripKey]; !exists {
			tripsSeen[tripKey] = struct{}{}

			// Parse date for weekday
			dateParts := strings.Split(dateStr, "-")
			if len(dateParts) == 3 {
				year, _ := strconv.Atoi(dateParts[0])
				month, _ := strconv.Atoi(dateParts[1])
				day, _ := strconv.Atoi(dateParts[2])
				t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)

				tripsByDate[dateKey] = append(tripsByDate[dateKey], TripInfo{
					Date:      dateKey,
					VehicleID: vehicleID,
					Type:      busType,
					Weekday:   t.Weekday(),
				})
			}
		}

		// Check if this is a "live" bus (within last 15 minutes)
		if !ts.Before(cutoff) {
			lat, _ := strconv.ParseFloat(fields[6], 64)
			lon, _ := strconv.ParseFloat(fields[7], 64)

			bus := LiveBus{
				VehicleID: vehicleID,
				Type:      busType,
				Route:     fields[4],
				LastSeen:  ts.In(pacificTZ),
				Lat:       lat,
				Lon:       lon,
			}

			// Keep only the most recent observation per vehicle
			if existing, ok := seen[vehicleID]; !ok || ts.After(existing.LastSeen) {
				seen[vehicleID] = bus
			}
		}
	}

	for _, bus := range seen {
		buses = append(buses, bus)
	}

	// Sort by vehicle ID
	sort.Slice(buses, func(i, j int) bool {
		return buses[i].VehicleID < buses[j].VehicleID
	})

	return buses, tripsByDate, scanner.Err()
}

// TripInfo holds info about a single trip for aggregation
type TripInfo struct {
	Date      string
	VehicleID string
	Type      string
	Weekday   time.Weekday
}

func loadHistoricData(filename string) (map[string]*BusStats, map[string][]TripInfo, []string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, nil, nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, nil, nil, err
	}

	cols := make(map[string]int)
	for i, col := range header {
		cols[col] = i
	}

	busStats := make(map[string]*BusStats)
	tripsByDate := make(map[string][]TripInfo)
	tripVehicle := make(map[string]string) // tripKey -> vehicleID
	dateSet := make(map[string]struct{})

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}

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
		dateSet[serviceDate] = struct{}{}

		// Update bus stats
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

		// Track trips by date for aggregation
		weekday := parseServiceDate(serviceDate)
		tripsByDate[serviceDate] = append(tripsByDate[serviceDate], TripInfo{
			Date:      serviceDate,
			VehicleID: vehicleID,
			Type:      busType,
			Weekday:   weekday,
		})
	}

	// Convert date set to sorted slice
	validDates := make([]string, 0, len(dateSet))
	for date := range dateSet {
		validDates = append(validDates, date)
	}
	sort.Strings(validDates)

	return busStats, tripsByDate, validDates, nil
}

func parseServiceDate(date string) time.Weekday {
	if len(date) != 8 {
		return time.Sunday
	}
	year, _ := strconv.Atoi(date[0:4])
	month, _ := strconv.Atoi(date[4:6])
	day, _ := strconv.Atoi(date[6:8])
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return t.Weekday()
}

func serviceDateToTime(date string) time.Time {
	if len(date) != 8 {
		return time.Time{}
	}
	year, _ := strconv.Atoi(date[0:4])
	month, _ := strconv.Atoi(date[4:6])
	day, _ := strconv.Atoi(date[6:8])
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

func computeAllBEBStats(busStats map[string]*BusStats, validDates []string) []BusResult {
	totalValidDays := len(validDates)
	var results []BusResult

	bebIDs := []string{"1600", "1601", "1602", "1603", "1800", "1801", "1802", "1803"}
	for _, id := range bebIDs {
		if stats, ok := busStats[id]; ok {
			result := computeBusResult(stats, validDates, totalValidDays)
			results = append(results, result)
		} else {
			// Bus not found in data - create empty result
			results = append(results, BusResult{
				VehicleID: id,
				Type:      "BEB",
			})
		}
	}

	return results
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
	if totalValidDays > 0 {
		result.Availability = float64(serviceDays) / float64(totalValidDays) * 100
	}

	outageStart := ""
	outageLength := 0

	for i, date := range validDates {
		trips := stats.DayTripMaps[date]
		if trips > 0 {
			miles := deadheadMiles + tripMiles*float64(trips)
			result.TotalMiles += miles

			if outageLength >= 3 {
				result.FailureCount++
				endDate := validDates[i-1]
				result.OutageRanges = append(result.OutageRanges, OutageRange{Start: outageStart, End: endDate, Len: outageLength})
			}
			outageStart = ""
			outageLength = 0
		} else {
			result.OutOfService++
			if outageStart == "" {
				outageStart = date
			}
			outageLength++
		}
	}

	if outageLength >= 3 {
		result.FailureCount++
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

func computeGroupResult(name string, members []string, busStats map[string]*BusStats, validDates []string) BusResult {
	group := &BusStats{
		VehicleID:   name,
		Type:        "BEB",
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
	return computeBusResult(group, validDates, len(validDates))
}

func computeWeekendWeekdaySplit(tripsByDate map[string][]TripInfo) (weekend, weekday BEBDieselSplit) {
	for _, trips := range tripsByDate {
		for _, trip := range trips {
			isWeekend := trip.Weekday == time.Saturday || trip.Weekday == time.Sunday
			if isWeekend {
				if trip.Type == "BEB" {
					weekend.BEBTrips++
				} else {
					weekend.DieselTrips++
				}
			} else {
				if trip.Type == "BEB" {
					weekday.BEBTrips++
				} else {
					weekday.DieselTrips++
				}
			}
		}
	}

	weekend.TotalTrips = weekend.BEBTrips + weekend.DieselTrips
	if weekend.TotalTrips > 0 {
		weekend.BEBPct = float64(weekend.BEBTrips) / float64(weekend.TotalTrips) * 100
		weekend.DieselPct = float64(weekend.DieselTrips) / float64(weekend.TotalTrips) * 100
	}

	weekday.TotalTrips = weekday.BEBTrips + weekday.DieselTrips
	if weekday.TotalTrips > 0 {
		weekday.BEBPct = float64(weekday.BEBTrips) / float64(weekday.TotalTrips) * 100
		weekday.DieselPct = float64(weekday.DieselTrips) / float64(weekday.TotalTrips) * 100
	}

	return
}

func computeTimePeriodSplit(tripsByDate map[string][]TripInfo, start, end time.Time) BEBDieselSplit {
	var split BEBDieselSplit

	for dateStr, trips := range tripsByDate {
		date := serviceDateToTime(dateStr)
		if date.Before(start) || date.After(end) {
			continue
		}

		for _, trip := range trips {
			if trip.Type == "BEB" {
				split.BEBTrips++
			} else {
				split.DieselTrips++
			}
		}
	}

	split.TotalTrips = split.BEBTrips + split.DieselTrips
	if split.TotalTrips > 0 {
		split.BEBPct = float64(split.BEBTrips) / float64(split.TotalTrips) * 100
		split.DieselPct = float64(split.DieselTrips) / float64(split.TotalTrips) * 100
	}

	return split
}

func computeWastedTime(tripsByDate map[string][]TripInfo, now time.Time) WastedTimeStats {
	var stats WastedTimeStats

	weekStart := now.AddDate(0, 0, -7)
	monthStart := now.AddDate(0, -1, 0)
	yearStart := now.AddDate(-1, 0, 0)

	for dateStr, trips := range tripsByDate {
		date := serviceDateToTime(dateStr)

		for _, trip := range trips {
			if trip.Type != "diesel" {
				continue
			}

			if !date.Before(yearStart) {
				stats.DieselTripsYear++
			}
			if !date.Before(monthStart) {
				stats.DieselTripsMonth++
			}
			if !date.Before(weekStart) {
				stats.DieselTripsWeek++
			}
		}
	}

	// Each one-way trip wastes 8.5 minutes (17 min per round trip / 2)
	wastedMinutesPerTrip := float64(chargingDwell) / 2.0

	stats.WastedMinutesWeek = float64(stats.DieselTripsWeek) * wastedMinutesPerTrip
	stats.WastedMinutesMonth = float64(stats.DieselTripsMonth) * wastedMinutesPerTrip
	stats.WastedMinutesYear = float64(stats.DieselTripsYear) * wastedMinutesPerTrip

	stats.WastedHoursWeek = stats.WastedMinutesWeek / 60.0
	stats.WastedHoursMonth = stats.WastedMinutesMonth / 60.0
	stats.WastedHoursYear = stats.WastedMinutesYear / 60.0

	// Potential extra trips if schedule optimized (each trip takes ~23 minutes)
	stats.PotentialExtraTrips = int(stats.WastedMinutesYear / float64(actualRouteTime))

	return stats
}

// findLastWeekendBEB finds the most recent weekend date when a BEB ran
func findLastWeekendBEB(tripsByDate map[string][]TripInfo) (dateStr string, vehicleID string) {
	var latestDate string
	var latestVehicle string

	// Known bad data points (coding errors, not actual BEB weekend service)
	badData := map[string]struct{}{
		"20250607|1803": {}, // June 7, 2025 - data error
	}

	for date, trips := range tripsByDate {
		for _, trip := range trips {
			if trip.Type != "BEB" {
				continue
			}
			if trip.Weekday != time.Saturday && trip.Weekday != time.Sunday {
				continue
			}
			// Skip known bad data
			key := date + "|" + trip.VehicleID
			if _, isBad := badData[key]; isBad {
				continue
			}
			// Found a BEB on weekend
			if date > latestDate {
				latestDate = date
				latestVehicle = trip.VehicleID
			}
		}
	}

	if latestDate != "" {
		return formatDateHuman(latestDate), latestVehicle
	}
	return "", ""
}

// findLastAnyBEB finds the most recent date when any BEB ran (weekday or weekend)
func findLastAnyBEB(tripsByDate map[string][]TripInfo) (dateStr string, vehicleID string) {
	var latestDate string
	var latestVehicle string

	// Known bad data points (coding errors, not actual BEB service)
	badData := map[string]struct{}{
		"20250607|1803": {}, // June 7, 2025 - data error
	}

	for date, trips := range tripsByDate {
		for _, trip := range trips {
			if trip.Type != "BEB" {
				continue
			}
			// Skip known bad data
			key := date + "|" + trip.VehicleID
			if _, isBad := badData[key]; isBad {
				continue
			}
			// Found a BEB trip
			if date > latestDate {
				latestDate = date
				latestVehicle = trip.VehicleID
			}
		}
	}

	if latestDate != "" {
		return latestDate, latestVehicle
	}
	return "", ""
}

// formatDateHuman converts YYYYMMDD to "January 2, 2006" format
func formatDateHuman(date string) string {
	if len(date) != 8 {
		return date
	}
	year, _ := strconv.Atoi(date[0:4])
	month, _ := strconv.Atoi(date[4:6])
	day, _ := strconv.Atoi(date[6:8])
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return t.Format("January 2, 2006")
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

func valueAt(record []string, idx int) string {
	if idx >= 0 && idx < len(record) {
		return record[idx]
	}
	return ""
}

func formatDate(date string) string {
	if len(date) != 8 {
		return date
	}
	return date[0:4] + "-" + date[4:6] + "-" + date[6:8]
}

// isAtWalnutCreekBART checks if a point is inside the Walnut Creek BART bus area polygon
// using the ray casting algorithm.
func isAtWalnutCreekBART(lat, lon float64) bool {
	n := len(walnutCreekBARTPolygon)
	inside := false

	j := n - 1
	for i := 0; i < n; i++ {
		yi, xi := walnutCreekBARTPolygon[i][0], walnutCreekBARTPolygon[i][1]
		yj, xj := walnutCreekBARTPolygon[j][0], walnutCreekBARTPolygon[j][1]

		if ((yi > lat) != (yj > lat)) &&
			(lon < (xj-xi)*(lat-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

func generateHTML(data DashboardData, outputPath string) error {
	tmpl, err := template.New("dashboard").Funcs(template.FuncMap{
		"formatTime": func(t time.Time) string {
			return t.Format("2006-01-02 3:04 PM MST")
		},
		"formatTimeHuman": func(t time.Time) string {
			now := time.Now().In(pacificTZ)
			t = t.In(pacificTZ)
			diff := now.Sub(t)

			if diff < time.Minute {
				return "just now"
			} else if diff < time.Hour {
				mins := int(diff.Minutes())
				if mins == 1 {
					return "1 minute ago"
				}
				return fmt.Sprintf("%d minutes ago", mins)
			} else if diff < 24*time.Hour {
				hours := int(diff.Hours())
				if hours == 1 {
					return "1 hour ago"
				}
				return fmt.Sprintf("%d hours ago", hours)
			}
			// More than a day ago, show date and time
			if t.Year() == now.Year() {
				return t.Format("Jan 2 at 3:04 PM")
			}
			return t.Format("Jan 2, 2006 at 3:04 PM")
		},
		"formatPct": func(f float64) string {
			return fmt.Sprintf("%.1f%%", f)
		},
		"formatFloat": func(f float64) string {
			return fmt.Sprintf("%.1f", f)
		},
		"formatCoord": func(f float64) string {
			return fmt.Sprintf("%.5f", f)
		},
		"mapsURL": func(lat, lon float64) string {
			return fmt.Sprintf("https://www.google.com/maps?q=%.6f,%.6f", lat, lon)
		},
		"gtZero": func(f float64) bool {
			return f > 0
		},
		"formatHours": func(f float64) string {
			return fmt.Sprintf("%.0f", f)
		},
		"formatDate": func(date string) string {
			return formatDate(date)
		},
		"busTypeClass": func(t string) string {
			if t == "BEB" {
				return "beb"
			}
			return "diesel"
		},
		"isAtWalnutCreekBART": func(lat, lon float64) bool {
			return isAtWalnutCreekBART(lat, lon)
		},
		"walnutCreekBARTURL": func() string {
			return fmt.Sprintf("https://www.google.com/maps?q=%.6f,%.6f", walnutCreekBARTCenter[0], walnutCreekBARTCenter[1])
		},
	}).Parse(htmlTemplate)
	if err != nil {
		return err
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer file.Close()

	return tmpl.Execute(file, data)
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>County Connection Routes {{.Routes}} Dashboard</title>
    <style>
        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            line-height: 1.6;
            color: #333;
            max-width: 1200px;
            margin: 0 auto;
            padding: 20px;
            background: #f5f5f5;
        }
        header {
            background: linear-gradient(135deg, #2c5282 0%, #1a365d 100%);
            color: white;
            padding: 30px;
            border-radius: 8px;
            margin-bottom: 30px;
        }
        header h1 {
            font-size: 2rem;
            margin-bottom: 8px;
        }
        header p {
            opacity: 0.9;
            font-size: 0.95rem;
        }
        .section {
            background: white;
            border-radius: 8px;
            padding: 24px;
            margin-bottom: 24px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        .section h2 {
            color: #2c5282;
            margin-bottom: 16px;
            padding-bottom: 8px;
            border-bottom: 2px solid #e2e8f0;
        }
        table {
            width: 100%;
            border-collapse: collapse;
            margin-top: 12px;
        }
        th, td {
            padding: 12px;
            text-align: left;
            border-bottom: 1px solid #e2e8f0;
        }
        th {
            background: #f7fafc;
            font-weight: 600;
            color: #4a5568;
        }
        tr:hover {
            background: #f7fafc;
        }
        .beb {
            color: #276749;
            font-weight: 600;
        }
        .beb-row {
            background: #f0fff4;
        }
        .beb-row:hover {
            background: #c6f6d5;
        }
        .diesel {
            color: #744210;
        }
        .diesel-row {
            background: #fffaf0;
        }
        .diesel-row:hover {
            background: #feebc8;
        }
        .stat-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 16px;
            margin-top: 16px;
        }
        .stat-card {
            background: #f7fafc;
            padding: 16px;
            border-radius: 6px;
            text-align: center;
        }
        .stat-value {
            font-size: 2rem;
            font-weight: 700;
            color: #2c5282;
        }
        .stat-label {
            font-size: 0.875rem;
            color: #718096;
            margin-top: 4px;
        }
        .bar-container {
            background: #e2e8f0;
            border-radius: 4px;
            height: 24px;
            display: flex;
            overflow: hidden;
            margin-top: 8px;
        }
        .bar-beb {
            background: #48bb78;
            height: 100%;
            display: flex;
            align-items: center;
            justify-content: center;
            color: white;
            font-size: 0.75rem;
            font-weight: 600;
        }
        .bar-diesel {
            background: #ed8936;
            height: 100%;
            display: flex;
            align-items: center;
            justify-content: center;
            color: white;
            font-size: 0.75rem;
            font-weight: 600;
        }
        .live-indicator {
            display: inline-block;
            width: 10px;
            height: 10px;
            background: #48bb78;
            border-radius: 50%;
            margin-right: 8px;
            animation: pulse 2s infinite;
        }
        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50% { opacity: 0.5; }
        }
        .no-data {
            color: #a0aec0;
            font-style: italic;
        }
        .outage-list {
            font-size: 0.875rem;
            color: #718096;
            margin-top: 4px;
        }
        .wasted-highlight {
            background: #fff5f5;
            border-left: 4px solid #fc8181;
            padding: 16px;
            margin-top: 16px;
            border-radius: 0 6px 6px 0;
        }
        .split-comparison {
            display: grid;
            grid-template-columns: 1fr 1fr;
            gap: 24px;
        }
        @media (max-width: 768px) {
            .split-comparison {
                grid-template-columns: 1fr;
            }
            .stat-grid {
                grid-template-columns: 1fr 1fr;
            }
        }
        footer {
            text-align: center;
            color: #718096;
            padding: 20px;
            font-size: 0.875rem;
        }
    </style>
</head>
<body>
    <header>
        <h1>County Connection Routes {{.Routes}} Dashboard</h1>
        <p>Battery Electric Bus (BEB) vs Diesel Bus Analysis | Generated: {{formatTime .GeneratedAt}}</p>
        <p>Data range: {{.DataStartDate}} to {{.DataEndDate}} ({{.TotalValidDays}} days)</p>
    </header>

    <div class="section">
        <h2><span class="live-indicator"></span>Live Status</h2>
        {{if .LiveBuses}}
        <p>Currently active buses on routes {{.Routes}} (last 15 minutes):</p>
        <div class="stat-grid">
            <div class="stat-card">
                <div class="stat-value beb">{{.LiveBEBCount}}</div>
                <div class="stat-label">BEB Running</div>
            </div>
            <div class="stat-card">
                <div class="stat-value diesel">{{.LiveDieselCount}}</div>
                <div class="stat-label">Diesel Running</div>
            </div>
        </div>
        <table>
            <thead>
                <tr>
                    <th>Vehicle ID</th>
                    <th>Type</th>
                    <th>Route</th>
                    <th>Last Seen</th>
                    <th>Location</th>
                </tr>
            </thead>
            <tbody>
                {{range .LiveBuses}}
                <tr class="{{busTypeClass .Type}}-row">
                    <td><strong>{{.VehicleID}}</strong></td>
                    <td class="{{busTypeClass .Type}}">{{.Type}}</td>
                    <td>{{.Route}}</td>
                    <td>{{formatTimeHuman .LastSeen}}</td>
                    <td>{{if isAtWalnutCreekBART .Lat .Lon}}<a href="{{walnutCreekBARTURL}}" target="_blank" rel="noopener">Walnut Creek BART</a>{{else}}<a href="{{mapsURL .Lat .Lon}}" target="_blank" rel="noopener">{{formatCoord .Lat}}, {{formatCoord .Lon}}</a>{{end}}</td>
                </tr>
                {{end}}
            </tbody>
        </table>
        {{else}}
        <p class="no-data">No buses currently active (or tracking data unavailable)</p>
        {{end}}
        <p><b>Why this matters:</b> Both of these routes build
        in 15-20 minutes of downtime into their schedules <a
        href="https://www.seamlessbayarea.org/blog/2025/11/7/changes-to-an-east-bay-bus-route-should-focus-on-frequency-improved-connections">to
        wirelessly recharge battery electric buses at BART</a>. However, the
        battery electric buses break down so often that they rarely ever run.
        If County Connection would commit to running diesel buses instead of
        battery electric buses, they could run bus service more frequently.</p>

        <p>
        Other bus agencies that have reduced headways from 45 to 30 minutes 
        (the 5 bus) or from 15 to 10 minutes (the 4 bus) have seen ridership
        increases of 10-25%% on those routes.
        </p>
    </div>

    <div class="section">
        <h2>Weekend vs Weekday Service</h2>
        <div class="split-comparison">
            <div>
                <h3>Weekend (Sat/Sun)</h3>
                <div class="bar-container">
                    {{if gt .WeekendSplit.TotalTrips 0}}
                    <div class="bar-beb" style="width: {{formatFloat .WeekendSplit.BEBPct}}%;">{{formatPct .WeekendSplit.BEBPct}}</div>
                    <div class="bar-diesel" style="width: {{formatFloat .WeekendSplit.DieselPct}}%;">{{formatPct .WeekendSplit.DieselPct}}</div>
                    {{end}}
                </div>
                <p style="margin-top: 8px;">
                    <span class="beb">{{.WeekendSplit.BEBTrips}} BEB trips</span> |
                    <span class="diesel">{{.WeekendSplit.DieselTrips}} diesel trips</span>
                </p>
            </div>
            <div>
                <h3>Weekday (Mon-Fri)</h3>
                <div class="bar-container">
                    {{if gt .WeekdaySplit.TotalTrips 0}}
                    <div class="bar-beb" style="width: {{formatFloat .WeekdaySplit.BEBPct}}%;">{{formatPct .WeekdaySplit.BEBPct}}</div>
                    <div class="bar-diesel" style="width: {{formatFloat .WeekdaySplit.DieselPct}}%;">{{formatPct .WeekdaySplit.DieselPct}}</div>
                    {{end}}
                </div>
                <p style="margin-top: 8px;">
                    <span class="beb">{{.WeekdaySplit.BEBTrips}} BEB trips</span> |
                    <span class="diesel">{{.WeekdaySplit.DieselTrips}} diesel trips</span>
                </p>
            </div>
        </div>
        {{if .LastWeekendBEB}}
        <div class="wasted-highlight" style="margin-top: 16px; background: #f0fff4; border-left-color: #48bb78;">
            <p><strong>Last weekend BEB service:</strong> {{.LastWeekendBEB}} (Bus {{.LastWeekendBEBID}})</p>
        </div>
        {{else}}
        <div class="wasted-highlight" style="margin-top: 16px;">
            <p><strong>No BEB weekend service found in data.</strong></p>
        </div>
        {{end}}
    </div>

    <div class="section">
        <h2>BEB Fleet Reliability</h2>
        <p>Performance statistics for all 8 Battery Electric Buses (data since {{.DataStartDate}})</p>
        <table>
            <thead>
                <tr>
                    <th>Vehicle</th>
                    <th>Trips</th>
                    <th>Service Days</th>
                    <th>Availability</th>
                    <th>Failures</th>
                    <th>MDBF (miles)</th>
                </tr>
            </thead>
            <tbody>
                {{range .BEBStats}}
                <tr class="beb-row">
                    <td><strong>{{.VehicleID}}</strong></td>
                    <td>{{.TripCount}}</td>
                    <td>{{.ServiceDays}}</td>
                    <td>{{formatPct .Availability}}</td>
                    <td>{{.FailureCount}}</td>
                    <td>{{if gtZero .MDBF}}{{formatFloat .MDBF}}{{else}}-{{end}}</td>
                </tr>
                {{end}}
            </tbody>
        </table>

        <h3 style="margin-top: 24px;">Group Summaries</h3>
        <table>
            <thead>
                <tr>
                    <th>Group</th>
                    <th>Trips</th>
                    <th>Service Days</th>
                    <th>Availability</th>
                    <th>Failures</th>
                    <th>MDBF (miles)</th>
                </tr>
            </thead>
            <tbody>
                <tr class="beb-row">
                    <td><strong>{{.Group1600Stats.VehicleID}}</strong> (green/WC livery)</td>
                    <td>{{.Group1600Stats.TripCount}}</td>
                    <td>{{.Group1600Stats.ServiceDays}}</td>
                    <td>{{formatPct .Group1600Stats.Availability}}</td>
                    <td>{{.Group1600Stats.FailureCount}}</td>
                    <td>{{if gtZero .Group1600Stats.MDBF}}{{formatFloat .Group1600Stats.MDBF}}{{else}}-{{end}}</td>
                </tr>
                <tr class="beb-row">
                    <td><strong>{{.Group1800Stats.VehicleID}}</strong> (white BEB)</td>
                    <td>{{.Group1800Stats.TripCount}}</td>
                    <td>{{.Group1800Stats.ServiceDays}}</td>
                    <td>{{formatPct .Group1800Stats.Availability}}</td>
                    <td>{{.Group1800Stats.FailureCount}}</td>
                    <td>{{if gtZero .Group1800Stats.MDBF}}{{formatFloat .Group1800Stats.MDBF}}{{else}}-{{end}}</td>
                </tr>
            </tbody>
        </table>
    </div>

    <div class="section">
        <h2>BEB vs Diesel by Time Period</h2>
        <div class="stat-grid">
            <div class="stat-card">
                <h4>Last 7 Days</h4>
                <div class="bar-container">
                    {{if gt .WeekSplit.TotalTrips 0}}
                    <div class="bar-beb" style="width: {{formatFloat .WeekSplit.BEBPct}}%;">{{formatPct .WeekSplit.BEBPct}}</div>
                    <div class="bar-diesel" style="width: {{formatFloat .WeekSplit.DieselPct}}%;">{{formatPct .WeekSplit.DieselPct}}</div>
                    {{end}}
                </div>
                <p style="margin-top: 8px; font-size: 0.875rem;">
                    {{.WeekSplit.BEBTrips}} BEB / {{.WeekSplit.DieselTrips}} diesel
                </p>
            </div>
            <div class="stat-card">
                <h4>Last 30 Days</h4>
                <div class="bar-container">
                    {{if gt .MonthSplit.TotalTrips 0}}
                    <div class="bar-beb" style="width: {{formatFloat .MonthSplit.BEBPct}}%;">{{formatPct .MonthSplit.BEBPct}}</div>
                    <div class="bar-diesel" style="width: {{formatFloat .MonthSplit.DieselPct}}%;">{{formatPct .MonthSplit.DieselPct}}</div>
                    {{end}}
                </div>
                <p style="margin-top: 8px; font-size: 0.875rem;">
                    {{.MonthSplit.BEBTrips}} BEB / {{.MonthSplit.DieselTrips}} diesel
                </p>
            </div>
            <div class="stat-card">
                <h4>Last 365 Days</h4>
                <div class="bar-container">
                    {{if gt .YearSplit.TotalTrips 0}}
                    <div class="bar-beb" style="width: {{formatFloat .YearSplit.BEBPct}}%;">{{formatPct .YearSplit.BEBPct}}</div>
                    <div class="bar-diesel" style="width: {{formatFloat .YearSplit.DieselPct}}%;">{{formatPct .YearSplit.DieselPct}}</div>
                    {{end}}
                </div>
                <p style="margin-top: 8px; font-size: 0.875rem;">
                    {{.YearSplit.BEBTrips}} BEB / {{.YearSplit.DieselTrips}} diesel
                </p>
            </div>
        </div>
        {{if .ShowBEBDrought}}
        <div class="wasted-highlight" style="margin-top: 16px;">
            <p><strong>No BEB has run on Routes {{.Routes}} for {{.DaysSinceLastBEB}} days.</strong> The last BEB service was on {{.LastAnyBEB}} (Bus {{.LastAnyBEBID}}).</p>
        </div>
        {{end}}
    </div>

    <div class="section">
        <h2>Diesel "Wasted Time" at BART</h2>
        <p>The Route 4 schedule allocates 40-minute round trips, but actual driving time is only 23 minutes. The extra 17 minutes per round trip is designed for BEB wireless charging at Walnut Creek BART. Diesel buses don't need this charging time but must wait anyway.</p>

        <div class="wasted-highlight">
            <div class="stat-grid">
                <div class="stat-card">
                    <div class="stat-value">{{formatHours .WastedTime.WastedHoursWeek}}</div>
                    <div class="stat-label">Hours wasted (7 days)</div>
                    <div class="stat-label">{{.WastedTime.DieselTripsWeek}} diesel trips</div>
                </div>
                <div class="stat-card">
                    <div class="stat-value">{{formatHours .WastedTime.WastedHoursMonth}}</div>
                    <div class="stat-label">Hours wasted (30 days)</div>
                    <div class="stat-label">{{.WastedTime.DieselTripsMonth}} diesel trips</div>
                </div>
                <div class="stat-card">
                    <div class="stat-value">{{formatHours .WastedTime.WastedHoursYear}}</div>
                    <div class="stat-label">Hours wasted (365 days)</div>
                    <div class="stat-label">{{.WastedTime.DieselTripsYear}} diesel trips</div>
                </div>
            </div>
        </div>

        <p style="margin-top: 16px;">
            <strong>Potential impact:</strong> If diesel buses could run without the 17-minute charging dwell,
            the wasted time from the past year could have provided approximately
            <strong>{{.WastedTime.PotentialExtraTrips}} additional one-way trips</strong>.
        </p>
    </div>

    <footer>
        <p>Data sources: 511.org GTFS Realtime API (live tracking) and GTFS Historic Data (reliability analysis)</p>
        <p>Analysis methodology based on <a href="https://seamlessbayarea.org">Seamless Bay Area</a> research</p>
    </footer>
</body>
</html>
`
