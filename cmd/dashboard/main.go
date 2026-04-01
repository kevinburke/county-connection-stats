package main

import (
	"bufio"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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

	// Route 5 specific BEB tracking
	LastRoute5BEB      string // Last date a BEB ran on Route 5, empty if never
	LastRoute5BEBID    string // Vehicle ID of the BEB
	DaysSinceRoute5BEB int    // Number of days since last BEB run on Route 5

	// Route 4 round trip time charts (before/after March 29 route change)
	TripTimeChartsJSON template.JS

	// Route 4 new-route summary stats
	NewRouteMedianRT     int     // median round trip time (minutes) after cutoff
	NewRouteIdlePct      float64 // median idle as % of 45-min cycle
	NewRouteAllAtBARTAvg float64 // avg minutes per weekday all 3 route-4 buses at BART
	NewRoute2AtBARTAvg   float64 // avg minutes per weekday 2+ route-4 buses at BART
	NewRoute2AtBARTPct   float64 // percentage of weekday service time with 2+ at BART
	NewRouteWeekdayCount int     // number of weekdays in after period

	// Before/after idle vs in-service breakdown
	BeforeIdleHours  float64
	BeforeRouteHours float64
	AfterIdleHours   float64
	AfterRouteHours  float64
}

// RoundTrip represents one BART departure/return cycle.
type RoundTrip struct {
	Vehicle   string
	Departure time.Time
	Duration  int // minutes
}

func (rt RoundTrip) IsWeekend() bool {
	wd := rt.Departure.Weekday()
	return wd == time.Saturday || wd == time.Sunday
}

func (rt RoundTrip) IsPeak() bool {
	hour := rt.Departure.Hour()
	return (hour >= 7 && hour < 9) || (hour >= 16 && hour < 19)
}

// TripTimeChart holds histogram data for one Flot chart.
type TripTimeChart struct {
	Title        string   `json:"Title"`
	BeforeData   [][2]int `json:"BeforeData"`
	AfterData    [][2]int `json:"AfterData"`
	BeforeCount  int      `json:"BeforeCount"`
	AfterCount   int      `json:"AfterCount"`
	BeforeMedian int      `json:"BeforeMedian"`
	AfterMedian  int      `json:"AfterMedian"`
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
	VehicleID       string
	Type            string
	TripCount       int
	ServiceDays     int
	Availability    float64
	OutOfService    int
	OutageRanges    []OutageRange
	TotalMiles      float64
	FailureCount    int
	MDBF            float64
	LastServiceDate string // YYYYMMDD format, most recent day this bus ran
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

	// Merge realtime trips into tripsByDate and busStats
	validDates = mergeRealtimeTrips(realtimeTrips, tripsByDate, busStats, validDates)
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

	// Find last Route 5 BEB run
	lastRoute5Date, lastRoute5Vehicle := findLastRouteBEB(tripsByDate, "5")
	if lastRoute5Date != "" {
		data.LastRoute5BEB = formatDateHuman(lastRoute5Date)
		data.LastRoute5BEBID = lastRoute5Vehicle
		lastRoute5Time := serviceDateToTime(lastRoute5Date)
		data.DaysSinceRoute5BEB = int(time.Since(lastRoute5Time).Hours() / 24)
	}

	// Calculate time period splits
	now := time.Now()
	data.WeekSplit = computeTimePeriodSplit(tripsByDate, now.AddDate(0, 0, -7), now)
	data.MonthSplit = computeTimePeriodSplit(tripsByDate, now.AddDate(0, -1, 0), now)
	data.YearSplit = computeTimePeriodSplit(tripsByDate, now.AddDate(-1, 0, 0), now)

	// Calculate wasted time
	data.WastedTime = computeWastedTime(tripsByDate, now)

	// Compute Route 4 round trip time charts (before/after March 29 route change)
	route4Cutoff := time.Date(2026, 3, 29, 0, 0, 0, 0, pacificTZ)
	roundTrips, err := loadRoundTrips(*trackingFile, "4")
	if err != nil {
		slog.Warn("Could not load round trip data", "error", err)
	} else {
		charts := computeTripTimeCharts(roundTrips, route4Cutoff)
		chartsJSON, err := json.Marshal(charts)
		if err != nil {
			slog.Warn("Could not marshal chart data", "error", err)
		} else {
			data.TripTimeChartsJSON = template.JS(chartsJSON)
		}

		// Compute median round trip and idle % for "after" period,
		// and before/after idle vs route hours breakdown.
		//
		// Per-bus approach: for each bus on each day, service time runs
		// from first BART departure to last BART return. Idle time is
		// service time minus time spent on route (sum of round trip
		// durations). This avoids assumptions about bus count or
		// service window.
		const cycleTime = 45.0 // minutes per cycle (3 buses * 15 min headway)

		// busDay tracks per-bus per-day stats.
		type busDay struct {
			firstDepart time.Time
			lastReturn  time.Time
			routeMin    float64
		}
		busDays := make(map[string]*busDay) // key: "date|vehicle"
		var afterDurations []int

		for _, rt := range roundTrips {
			day := rt.Departure.Format("2006-01-02")
			returnTime := rt.Departure.Add(time.Duration(rt.Duration) * time.Minute)
			key := day + "|" + rt.Vehicle
			bd := busDays[key]
			if bd == nil {
				bd = &busDay{firstDepart: rt.Departure, lastReturn: returnTime}
				busDays[key] = bd
			}
			if rt.Departure.Before(bd.firstDepart) {
				bd.firstDepart = rt.Departure
			}
			if returnTime.After(bd.lastReturn) {
				bd.lastReturn = returnTime
			}
			bd.routeMin += float64(rt.Duration)

			if !rt.Departure.Before(route4Cutoff) {
				afterDurations = append(afterDurations, rt.Duration)
			}
		}
		if len(afterDurations) > 0 {
			sort.Ints(afterDurations)
			data.NewRouteMedianRT = afterDurations[len(afterDurations)/2]
			medianIdle := cycleTime - float64(data.NewRouteMedianRT)
			data.NewRouteIdlePct = medianIdle / cycleTime * 100
		}

		// Sum up before/after route and idle hours from per-bus data.
		for key, bd := range busDays {
			day := key[:10]
			serviceMin := bd.lastReturn.Sub(bd.firstDepart).Minutes()
			idleMin := serviceMin - bd.routeMin
			if idleMin < 0 {
				idleMin = 0
			}
			dayDate, _ := time.ParseInLocation("2006-01-02", day, pacificTZ)
			if dayDate.Before(route4Cutoff) {
				data.BeforeRouteHours += bd.routeMin / 60
				data.BeforeIdleHours += idleMin / 60
			} else {
				data.AfterRouteHours += bd.routeMin / 60
				data.AfterIdleHours += idleMin / 60
			}
		}

		slog.Info("Round trip data loaded", "total", len(roundTrips))
	}

	// Compute Route 4 idle time and all-at-BART stats
	bartStats, err := computeRoute4BARTStats(*trackingFile, route4Cutoff)
	if err != nil {
		slog.Warn("Could not compute BART stats", "error", err)
	} else {
		data.NewRouteAllAtBARTAvg = bartStats.AllAtBARTAvgMinutes
		data.NewRoute2AtBARTAvg = bartStats.TwoAtBARTAvgMinutes
		data.NewRoute2AtBARTPct = bartStats.TwoAtBARTPctService
		data.NewRouteWeekdayCount = bartStats.WeekdayCount
		slog.Info("BART idle stats computed",
			"3atBARTAvg", bartStats.AllAtBARTAvgMinutes,
			"2atBARTAvg", bartStats.TwoAtBARTAvgMinutes,
			"2atBARTPct", bartStats.TwoAtBARTPctService,
			"weekdays", bartStats.WeekdayCount,
		)
	}

	// Generate HTML
	if err := generateHTML(data, *outputFile); err != nil {
		slog.Error("Failed to generate HTML", "error", err)
		os.Exit(1)
	}

	slog.Info("Dashboard generated", "output", *outputFile)
}

// openTrackingFiles finds all vehicle-tracking.tsv files matching the given
// path (and any rotated archives like .gz variants and date-suffixed files in
// the same directory), and returns a combined reader plus a cleanup function.
// Files are concatenated in lexicographic order; headers after the first file
// are skipped by the callers (they skip line 1 per file, but since we
// concatenate we only have one header at the top — rotated files don't have
// headers).
func openTrackingFiles(trackingPath string) (io.Reader, func(), error) {
	dir := filepath.Dir(trackingPath)
	base := filepath.Base(trackingPath)

	// Glob for the base file and any rotated variants (e.g. .tsv-2026-04, .tsv-2026-03.gz)
	pattern := filepath.Join(dir, base+"*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, nil, err
	}
	if len(matches) == 0 {
		return nil, nil, fmt.Errorf("no files matching %s", pattern)
	}
	sort.Strings(matches)

	var readers []io.Reader
	var closers []io.Closer

	for _, path := range matches {
		f, err := os.Open(path)
		if err != nil {
			// Close anything already opened
			for _, c := range closers {
				c.Close()
			}
			return nil, nil, fmt.Errorf("opening %s: %w", path, err)
		}
		closers = append(closers, f)

		if strings.HasSuffix(path, ".gz") {
			gz, err := gzip.NewReader(f)
			if err != nil {
				for _, c := range closers {
					c.Close()
				}
				return nil, nil, fmt.Errorf("decompressing %s: %w", path, err)
			}
			closers = append(closers, gz)
			readers = append(readers, gz)
		} else {
			readers = append(readers, f)
		}
		slog.Info("Loading tracking file", "path", path)
	}

	cleanup := func() {
		// Close in reverse order (gz readers before their underlying files)
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i].Close()
		}
	}

	return io.MultiReader(readers...), cleanup, nil
}

// loadTrackingData loads both live buses (last 15 min) and all trip data from tracking files.
func loadTrackingData(filename string) ([]LiveBus, map[string][]TripInfo, error) {
	r, cleanup, err := openTrackingFiles(filename)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()

	var buses []LiveBus
	seen := make(map[string]LiveBus)
	tripsByDate := make(map[string][]TripInfo)
	tripsSeen := make(map[string]struct{}) // track unique trips per day
	cutoff := time.Now().Add(-15 * time.Minute)

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "timestamp") {
			continue // skip header
		}
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

			route := fields[4] // route column

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
					Route:     route,
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
	Route     string
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
		routeID := valueAt(record, cols["route_id"])
		tripsByDate[serviceDate] = append(tripsByDate[serviceDate], TripInfo{
			Date:      serviceDate,
			VehicleID: vehicleID,
			Type:      busType,
			Weekday:   weekday,
			Route:     routeID,
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

// mergeRealtimeTrips merges live tracking data into tripsByDate and busStats,
// and returns an updated validDates slice (unsorted).
func mergeRealtimeTrips(realtimeTrips map[string][]TripInfo, tripsByDate map[string][]TripInfo, busStats map[string]*BusStats, validDates []string) []string {
	for dateStr, trips := range realtimeTrips {
		tripsByDate[dateStr] = append(tripsByDate[dateStr], trips...)
		if !slices.Contains(validDates, dateStr) {
			validDates = append(validDates, dateStr)
		}
		// Update per-vehicle busStats so LastServiceDate reflects live data
		for _, trip := range trips {
			bs := busStats[trip.VehicleID]
			if bs == nil {
				bs = &BusStats{
					VehicleID:   trip.VehicleID,
					Type:        trip.Type,
					DayTripMaps: make(map[string]int),
				}
				busStats[trip.VehicleID] = bs
			}
			bs.TripCount++
			bs.DayTripMaps[dateStr]++
		}
	}
	return validDates
}

// loadRoundTrips reads the tracking file and detects BART→BART round trips
// for the given route.
func loadRoundTrips(filename, routeFilter string) ([]RoundTrip, error) {
	r, cleanup, err := openTrackingFiles(filename)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	type vehicleState struct {
		wasAtBART     bool
		departureTime time.Time
	}
	states := make(map[string]*vehicleState)
	var roundTrips []RoundTrip

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "timestamp") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 10 {
			continue
		}

		ts, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		ts = ts.In(pacificTZ)
		vehicle := fields[3]
		obsRoute := fields[4]
		lat, _ := strconv.ParseFloat(fields[6], 64)
		lon, _ := strconv.ParseFloat(fields[7], 64)

		if obsRoute != routeFilter {
			continue
		}

		atBART := isAtWalnutCreekBART(lat, lon)

		state := states[vehicle]
		if state == nil {
			state = &vehicleState{wasAtBART: atBART}
			states[vehicle] = state
			continue
		}

		if state.wasAtBART && !atBART {
			state.departureTime = ts
		}
		if !state.wasAtBART && atBART && !state.departureTime.IsZero() {
			if state.departureTime.Format("2006-01-02") == ts.Format("2006-01-02") {
				duration := int(ts.Sub(state.departureTime).Minutes())
				if duration >= 10 && duration <= 90 {
					roundTrips = append(roundTrips, RoundTrip{
						Vehicle:   vehicle,
						Departure: state.departureTime,
						Duration:  duration,
					})
				}
			}
			state.departureTime = time.Time{}
		}
		state.wasAtBART = atBART
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	sort.Slice(roundTrips, func(i, j int) bool {
		return roundTrips[i].Departure.Before(roundTrips[j].Departure)
	})
	return roundTrips, nil
}

// computeTripTimeCharts splits round trips at cutoff and builds chart data.
func computeTripTimeCharts(roundTrips []RoundTrip, cutoff time.Time) []TripTimeChart {
	var before, after []RoundTrip
	for _, rt := range roundTrips {
		if rt.Departure.Before(cutoff) {
			before = append(before, rt)
		} else {
			after = append(after, rt)
		}
	}

	// Build histogram in 2-minute buckets. The tracking data polls every
	// ~2 minutes so durations are almost always even, leaving odd-minute
	// bars empty. Bucket label is the lower bound (e.g. 18 = 18-19 min).
	buildHist := func(trips []RoundTrip) [][2]int {
		buckets := make(map[int]int) // bucket start -> count
		for _, t := range trips {
			if t.Duration >= 10 && t.Duration <= 60 {
				bucket := t.Duration - (t.Duration % 2) // round down to even
				buckets[bucket]++
			}
		}
		var data [][2]int
		for i := 10; i <= 50; i += 2 {
			data = append(data, [2]int{i, buckets[i]})
		}
		return data
	}

	median := func(trips []RoundTrip) int {
		if len(trips) == 0 {
			return 0
		}
		d := make([]int, len(trips))
		for i, t := range trips {
			d[i] = t.Duration
		}
		sort.Ints(d)
		return d[len(d)/2]
	}

	makeChart := func(title string, b, a []RoundTrip) TripTimeChart {
		return TripTimeChart{
			Title:        title,
			BeforeData:   buildHist(b),
			AfterData:    buildHist(a),
			BeforeCount:  len(b),
			AfterCount:   len(a),
			BeforeMedian: median(b),
			AfterMedian:  median(a),
		}
	}

	filterWeekday := func(trips []RoundTrip) []RoundTrip {
		var out []RoundTrip
		for _, t := range trips {
			if !t.IsWeekend() {
				out = append(out, t)
			}
		}
		return out
	}
	filterWeekend := func(trips []RoundTrip) []RoundTrip {
		var out []RoundTrip
		for _, t := range trips {
			if t.IsWeekend() {
				out = append(out, t)
			}
		}
		return out
	}
	filterPeak := func(trips []RoundTrip) []RoundTrip {
		var out []RoundTrip
		for _, t := range trips {
			if t.IsPeak() && !t.IsWeekend() {
				out = append(out, t)
			}
		}
		return out
	}
	filterOffpeak := func(trips []RoundTrip) []RoundTrip {
		var out []RoundTrip
		for _, t := range trips {
			if !t.IsPeak() && !t.IsWeekend() {
				out = append(out, t)
			}
		}
		return out
	}

	return []TripTimeChart{
		makeChart("All Round Trips", before, after),
		makeChart("Weekday Only", filterWeekday(before), filterWeekday(after)),
		makeChart("Weekend Only", filterWeekend(before), filterWeekend(after)),
		makeChart("Peak Commute (7-9am, 4-7pm)", filterPeak(before), filterPeak(after)),
		makeChart("Off-Peak Weekday", filterOffpeak(before), filterOffpeak(after)),
	}
}

// Route4BARTStats holds measured idle time and all-at-BART stats for route 4.
type Route4BARTStats struct {
	MedianIdleMinutes   float64
	AllAtBARTAvgMinutes float64 // 3+ at BART, weekday avg
	TwoAtBARTAvgMinutes float64 // 2+ at BART, weekday avg
	TwoAtBARTPctService float64 // 2+ at BART as % of weekday service time
	WeekdayCount        int
}

// computeRoute4BARTStats reads the tracking file and computes idle time at BART
// (dwell between arrival and next departure) and minutes per day when all 3
// route 4 buses are at BART simultaneously. Only considers data on or after cutoff.
func computeRoute4BARTStats(filename string, cutoff time.Time) (Route4BARTStats, error) {
	r, cleanup, err := openTrackingFiles(filename)
	if err != nil {
		return Route4BARTStats{}, err
	}
	defer cleanup()

	type observation struct {
		ts      time.Time
		vehicle string
		atBART  bool
	}

	// Collect all route 4 observations after cutoff, sorted by time.
	var obs []observation

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "timestamp") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 10 {
			continue
		}
		if fields[4] != "4" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		ts = ts.In(pacificTZ)
		if ts.Before(cutoff) {
			continue
		}
		lat, _ := strconv.ParseFloat(fields[6], 64)
		lon, _ := strconv.ParseFloat(fields[7], 64)
		obs = append(obs, observation{
			ts:      ts,
			vehicle: fields[3],
			atBART:  isAtWalnutCreekBART(lat, lon),
		})
	}
	if err := scanner.Err(); err != nil {
		return Route4BARTStats{}, err
	}

	sort.Slice(obs, func(i, j int) bool { return obs[i].ts.Before(obs[j].ts) })

	// --- Idle time: measure dwell at BART for each vehicle ---
	// Track per-vehicle: when they arrived at BART, when they left.
	type vehicleState struct {
		wasAtBART   bool
		arrivalTime time.Time
	}
	states := make(map[string]*vehicleState)
	var idleTimes []float64

	for _, o := range obs {
		st := states[o.vehicle]
		if st == nil {
			st = &vehicleState{wasAtBART: o.atBART}
			if o.atBART {
				st.arrivalTime = o.ts
			}
			states[o.vehicle] = st
			continue
		}
		if !st.wasAtBART && o.atBART {
			// Arrived at BART
			st.arrivalTime = o.ts
		}
		if st.wasAtBART && !o.atBART && !st.arrivalTime.IsZero() {
			// Departing BART — idle time is now minus arrival
			if st.arrivalTime.Format("2006-01-02") == o.ts.Format("2006-01-02") {
				idle := o.ts.Sub(st.arrivalTime).Minutes()
				if idle >= 1 && idle <= 60 {
					idleTimes = append(idleTimes, idle)
				}
			}
			st.arrivalTime = time.Time{}
		}
		st.wasAtBART = o.atBART
	}

	var stats Route4BARTStats
	if len(idleTimes) > 0 {
		sort.Float64s(idleTimes)
		stats.MedianIdleMinutes = idleTimes[len(idleTimes)/2]
	}

	// --- All 3 at BART simultaneously ---
	// A vehicle only counts once it has completed at least one round trip
	// that day (departed BART and returned). This excludes buses that are
	// staging, warming up, or parked but not yet in active service.
	type bartVehicleState struct {
		wasAtBART   bool
		departed    bool      // has left BART at least once today
		hasTripped  bool      // completed a round trip (left BART and returned)
		arrivalTime time.Time // when this bus last arrived at BART
		currentDay  string    // reset on day change
	}
	bartStates := make(map[string]*bartVehicleState)
	activeAtBART := make(map[string]struct{}) // vehicles eligible and at BART
	var prevTS time.Time
	var prevDay string
	allAtBARTByDay := make(map[string]float64)
	twoAtBARTByDay := make(map[string]float64)
	serviceDays := make(map[string]struct{})

	// If a bus sits at BART for longer than one cycle without departing,
	// it's done for the day (went home), not idling between trips.
	const doneForDayTimeout = 45 * time.Minute

	for _, o := range obs {
		day := o.ts.Format("2006-01-02")
		hour := o.ts.Hour()
		if hour < 5 || hour >= 23 {
			continue
		}

		// Reset all state on day boundary — buses leave BART overnight.
		if day != prevDay {
			clear(bartStates)
			clear(activeAtBART)
			prevDay = day
		}

		// Expire buses that have been at BART too long — done for day.
		for v := range activeAtBART {
			if bs := bartStates[v]; bs != nil && !bs.arrivalTime.IsZero() {
				if o.ts.Sub(bs.arrivalTime) >= doneForDayTimeout {
					delete(activeAtBART, v)
					bs.hasTripped = false
				}
			}
		}

		serviceDays[day] = struct{}{}

		// Accumulate time for the interval BEFORE this observation.
		if !prevTS.IsZero() && prevTS.Format("2006-01-02") == day {
			elapsed := o.ts.Sub(prevTS).Minutes()
			if elapsed > 0 && elapsed <= 10 {
				if len(activeAtBART) >= 3 {
					allAtBARTByDay[day] += elapsed
				}
				if len(activeAtBART) >= 2 {
					twoAtBARTByDay[day] += elapsed
				}
			}
		}
		prevTS = o.ts

		// Update per-vehicle state.
		bs := bartStates[o.vehicle]
		if bs == nil {
			bs = &bartVehicleState{currentDay: day}
			bartStates[o.vehicle] = bs
		}

		// State machine: bus must leave BART then return to count as
		// having completed a trip. First observation at BART doesn't
		// count — the bus might be staging before service starts.
		if bs.wasAtBART && !o.atBART {
			bs.departed = true
			bs.arrivalTime = time.Time{}
		}
		if !bs.wasAtBART && o.atBART {
			bs.arrivalTime = o.ts
			if bs.departed {
				bs.hasTripped = true
			}
		}

		// Only count in the active set if the bus has made a trip.
		if bs.hasTripped && o.atBART {
			activeAtBART[o.vehicle] = struct{}{}
		} else {
			delete(activeAtBART, o.vehicle)
		}

		bs.wasAtBART = o.atBART
	}

	// Compute weekday-only averages. Service window is 6:52am-9:02pm = 850 min.
	const weekdayServiceMinutes = 850.0
	var weekdays []string
	for day := range serviceDays {
		year, _ := strconv.Atoi(day[0:4])
		month, _ := strconv.Atoi(day[5:7])
		d, _ := strconv.Atoi(day[8:10])
		wd := time.Date(year, time.Month(month), d, 0, 0, 0, 0, time.UTC).Weekday()
		if wd != time.Saturday && wd != time.Sunday {
			weekdays = append(weekdays, day)
		}
	}
	stats.WeekdayCount = len(weekdays)
	if stats.WeekdayCount > 0 {
		total3 := 0.0
		total2 := 0.0
		for _, day := range weekdays {
			total3 += allAtBARTByDay[day]
			total2 += twoAtBARTByDay[day]
		}
		stats.AllAtBARTAvgMinutes = total3 / float64(stats.WeekdayCount)
		stats.TwoAtBARTAvgMinutes = total2 / float64(stats.WeekdayCount)
		stats.TwoAtBARTPctService = (total2 / float64(stats.WeekdayCount)) / weekdayServiceMinutes * 100
	}

	return stats, nil
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

	// Find last service date
	for date := range stats.DayTripMaps {
		if date > result.LastServiceDate {
			result.LastServiceDate = date
		}
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

// findLastRouteBEB finds the most recent date when a BEB ran on a specific route
func findLastRouteBEB(tripsByDate map[string][]TripInfo, targetRoute string) (dateStr string, vehicleID string) {
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
			// Check if this is the target route
			// Route can be "5" from tracking file or "CC:5" from historic data
			if trip.Route != targetRoute && trip.Route != "CC:"+targetRoute {
				continue
			}
			// Skip known bad data
			key := date + "|" + trip.VehicleID
			if _, isBad := badData[key]; isBad {
				continue
			}
			// Found a BEB trip on target route
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
		"formatMinPerDay": func(f float64) string {
			if f >= 60 {
				return fmt.Sprintf("%.1f hrs/day", f/60)
			}
			return fmt.Sprintf("%.0f min/day", f)
		},
		"divf": func(a, b, mult float64) float64 {
			if b == 0 {
				return 0
			}
			return a / b * mult
		},
		"addf": func(a, b float64) float64 {
			return a + b
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
        href="https://www.seamlessbayarea.org/blog/2025/11/7/changes-to-an-east-bay-bus-route-should-focus-on-frequency-improved-connections">
        to wirelessly recharge battery electric buses at BART</a>. However, the
        battery electric buses break down so often that they rarely ever run.
        If County Connection could just run diesel buses instead of battery
        electric buses, or charge the battery electric buses more quickly,
        or run a full day's service on an overnight charge they could reduce
        scheduled headways and run bus service more frequently.</p>

        <p>
        Other bus agencies that have reduced headways from 45 to 30 minutes 
        (the 5 bus) or from 15 to 10 minutes (the 4 bus) have seen ridership
        increases of 10-25% on those routes.
        </p>
    </div>

    {{if gt .NewRouteWeekdayCount 0}}
    <div class="section">
        <h2>Route 4 New Route Summary (since March 29, weekdays)</h2>
        <div class="stat-grid">
            <div class="stat-card">
                <div class="stat-value">{{.NewRouteMedianRT}} min</div>
                <div class="stat-label">Median round trip</div>
                <div class="stat-label">(25 min scheduled)</div>
            </div>
            <div class="stat-card">
                <div class="stat-value">{{formatFloat .NewRouteIdlePct}}%</div>
                <div class="stat-label">of each 45-min cycle spent idling at BART</div>
                <div class="stat-label">(not picking up passengers)</div>
            </div>
            <div class="stat-card">
                <div class="stat-value">{{formatMinPerDay .NewRoute2AtBARTAvg}}</div>
                <div class="stat-label">2+ in-service buses idling at BART</div>
                <div class="stat-label">({{formatFloat .NewRoute2AtBARTPct}}% of service time)</div>
            </div>
            <div class="stat-card">
                <div class="stat-value">{{formatMinPerDay .NewRouteAllAtBARTAvg}}</div>
                <div class="stat-label">All 3 in-service buses idling together at BART</div>
                <div class="stat-label">(avg across {{.NewRouteWeekdayCount}} weekdays)</div>
            </div>
        </div>

        {{if gt .BeforeRouteHours 0.0}}
        <h3 style="margin-top: 24px;">Bus Hours: On Route vs. Idling at BART</h3>
        <p style="font-size: 14px; color: #555;">
            Total bus-hours across 3 buses. "Idling" is total service time minus
            measured time on route (BART departure to BART return).
        </p>
        <div style="display:flex;gap:16px">
            <div style="flex:1">
                <div style="text-align:center;margin-bottom:8px;font-weight:bold;color:steelblue">Before (23 min scheduled RT)</div>
                <div class="bar-container" style="height:auto;align-items:center">
                    <div style="background:#4682b4;width:{{formatFloat (divf .BeforeRouteHours (addf .BeforeRouteHours .BeforeIdleHours) 100)}}%;padding:8px 12px;color:white;text-align:center;font-size:13px">On route: {{formatFloat .BeforeRouteHours}} hrs ({{formatHours (divf .BeforeRouteHours (addf .BeforeRouteHours .BeforeIdleHours) 100)}}%)</div>
                    <div style="background:#ccc;width:{{formatFloat (divf .BeforeIdleHours (addf .BeforeRouteHours .BeforeIdleHours) 100)}}%;padding:8px 12px;text-align:center;font-size:13px">Idle: {{formatFloat .BeforeIdleHours}} hrs ({{formatHours (divf .BeforeIdleHours (addf .BeforeRouteHours .BeforeIdleHours) 100)}}%)</div>
                </div>
            </div>
            <div style="flex:1">
                <div style="text-align:center;margin-bottom:8px;font-weight:bold;color:#dc4c3c">After (25 min scheduled RT)</div>
                <div class="bar-container" style="height:auto;align-items:center">
                    <div style="background:#dc4c3c;width:{{formatFloat (divf .AfterRouteHours (addf .AfterRouteHours .AfterIdleHours) 100)}}%;padding:8px 12px;color:white;text-align:center;font-size:13px">On route: {{formatFloat .AfterRouteHours}} hrs ({{formatHours (divf .AfterRouteHours (addf .AfterRouteHours .AfterIdleHours) 100)}}%)</div>
                    <div style="background:#ccc;width:{{formatFloat (divf .AfterIdleHours (addf .AfterRouteHours .AfterIdleHours) 100)}}%;padding:8px 12px;text-align:center;font-size:13px">Idle: {{formatFloat .AfterIdleHours}} hrs ({{formatHours (divf .AfterIdleHours (addf .AfterRouteHours .AfterIdleHours) 100)}}%)</div>
                </div>
            </div>
        </div>
        {{end}}
    </div>
    {{end}}

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
                    <th>Last In Service</th>
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
                    <td>{{if .LastServiceDate}}{{formatDate .LastServiceDate}}{{else}}-{{end}}</td>
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
        {{if and .LastRoute5BEB (gt .DaysSinceRoute5BEB 7)}}
        <div class="wasted-highlight" style="margin-top: 16px;">
            <p><strong>No BEB on Route 5 since {{.LastRoute5BEB}}</strong> ({{.DaysSinceRoute5BEB}} days ago, Bus {{.LastRoute5BEBID}}).</p>
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

    {{if .TripTimeChartsJSON}}
    <div class="section">
        <h2>Route 4 Round Trip Times: Before vs After Route Change</h2>
        <p>
            County Connection changed the Route 4 path on <strong>March 29, 2026</strong>.
            Scheduled round trip: <strong>23 min</strong> (before) &rarr; <strong>25 min</strong> (after).
            Dashed vertical lines show the scheduled round-trip time for each period.
        </p>
        <p style="font-size: 14px; color: #555;">
            <strong>Methodology:</strong> Round trip time is measured using GPS tracking data
            from the <a href="https://511.org/open-data/transit">511.org API</a>.
            We define a geographic polygon around the Walnut Creek BART bus area, then
            measure the elapsed time from when a bus departs (leaves the polygon) to when
            it returns (re-enters the polygon) on the same day. Trips shorter than 10
            minutes or longer than 90 minutes are excluded as outliers.
        </p>
        <div id="trip-time-charts"></div>
    </div>
    <script src="https://cdnjs.cloudflare.com/ajax/libs/jquery/3.7.1/jquery.min.js"></script>
    <script src="https://cdnjs.cloudflare.com/ajax/libs/flot/0.8.3/jquery.flot.min.js"></script>
    <script>
    (function() {
        var charts = {{.TripTimeChartsJSON}};
        var container = document.getElementById("trip-time-charts");

        $.each(charts, function(i, chart) {
            if (chart.BeforeCount === 0 && chart.AfterCount === 0) return;

            var wrapper = document.createElement("div");
            wrapper.style.cssText = "background:white;border:1px solid #ddd;border-radius:4px;padding:20px;margin-bottom:20px;";

            var html = '<h3 style="margin-top:0">' + chart.Title + '</h3>' +
                '<div style="display:flex;gap:16px">' +
                  '<div style="flex:1">' +
                    '<div style="text-align:center;margin-bottom:4px;font-weight:bold;color:steelblue">' +
                      'Before: ' + chart.BeforeCount + ' trips, median ' + chart.BeforeMedian + ' min</div>' +
                    '<div id="flot-before-' + i + '" style="width:100%;height:300px"></div>' +
                  '</div>' +
                  '<div style="flex:1">' +
                    '<div style="text-align:center;margin-bottom:4px;font-weight:bold;color:#dc4c3c">' +
                      'After: ' + chart.AfterCount + ' trips, median ' + chart.AfterMedian + ' min</div>' +
                    '<div id="flot-after-' + i + '" style="width:100%;height:300px"></div>' +
                  '</div>' +
                '</div>';
            wrapper.innerHTML = html;
            container.appendChild(wrapper);

            var xaxisOpts = { min: 9, max: 51, tickSize: 4 };

            $.plot("#flot-before-" + i, [{
                data: chart.BeforeData,
                bars: { show: true, barWidth: 1.8, align: "center", fill: 0.7 },
                color: "steelblue"
            }], {
                xaxis: xaxisOpts,
                yaxis: { min: 0 },
                grid: {
                    markings: [
                        { color: "steelblue", lineWidth: 2, xaxis: { from: 23, to: 23 } }
                    ]
                }
            });

            $.plot("#flot-after-" + i, [{
                data: chart.AfterData,
                bars: { show: true, barWidth: 1.8, align: "center", fill: 0.7 },
                color: "#dc4c3c"
            }], {
                xaxis: xaxisOpts,
                yaxis: { min: 0 },
                grid: {
                    markings: [
                        { color: "#dc4c3c", lineWidth: 2, xaxis: { from: 25, to: 25 } }
                    ]
                }
            });
        });
    })();
    </script>
    {{end}}

    <footer>
        <p>Data sources: 511.org GTFS Realtime API (live tracking) and GTFS Historic Data (reliability analysis)</p>
        <p>Analysis methodology based on <a href="https://seamlessbayarea.org">Seamless Bay Area</a> research</p>
    </footer>
</body>
</html>
`
