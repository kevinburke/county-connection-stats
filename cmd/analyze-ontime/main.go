// analyze-ontime analyzes on-time performance for Route 4 and Route 5 buses.
//
// It reads the vehicle tracking TSV file and compares observed arrival times
// at terminus locations to the published schedule to determine how often
// buses are on time.
//
// Usage:
//
//	go run ./cmd/analyze-ontime -route 5
//	go run ./cmd/analyze-ontime -route 4
//
// Options:
//
//	-tracking-file   Vehicle tracking TSV file (default: vehicle-tracking.tsv)
//	-route           Route to analyze: 4 or 5 (default: 5)
//	-on-time-window  Minutes early/late to consider "on time" (default: 5)
//	-version         Print version and exit
package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const version = "0.2.0"

var (
	trackingFile = flag.String("tracking-file", "vehicle-tracking.tsv", "Vehicle tracking TSV file")
	routeFilter  = flag.String("route", "5", "Route to analyze (4 or 5)")
	onTimeWindow = flag.Int("on-time-window", 5, "Minutes early/late to consider on time")
	versionFlag  = flag.Bool("version", false, "Print version and exit")
	pacificTZ    *time.Location
)

// Walnut Creek BART bus area polygon (lat, lon pairs)
var walnutCreekBARTPolygon = [][2]float64{
	{37.90702176601014, -122.06856813397798},
	{37.90631565035961, -122.06699383073966},
	{37.90442920597368, -122.06813046461296},
	{37.90528481242584, -122.06935855178641},
}

// Creekside terminus polygon for Route 5 (lat, lon pairs)
var creeksidePolygon = [][2]float64{
	{37.88260858311487, -122.05108337231785},
	{37.88286182615337, -122.04946921154641},
	{37.88169300486415, -122.04886698642677},
	{37.88158391393072, -122.05094022044516},
}

// S Broadway and Mt Diablo terminus polygon for Route 4 (lat, lon pairs)
var sBroadwayPolygon = [][2]float64{
	{37.8979313200401, -122.05875803463786},
	{37.89797453351041, -122.05617324154504},
	{37.896094724101275, -122.055806332356},
	{37.8967602253355, -122.05894970361719},
}

// ScheduledTrip represents a scheduled departure/arrival
type ScheduledTrip struct {
	Direction string // direction identifier
	Stop      string // stop name
	Time      string // "HH:MM" in 24h format
}

// ArrivalEvent represents a detected arrival at a terminus
type ArrivalEvent struct {
	Date      string    // YYYY-MM-DD
	Time      time.Time // actual arrival time
	VehicleID string
	TripID    string
	Location  string // terminus name
	Scheduled string // matched scheduled time "HH:MM"
	DeltaMins int    // positive = late, negative = early
	Weekday   time.Weekday
}

func main() {
	flag.Parse()

	if *versionFlag {
		fmt.Printf("analyze-ontime version %s\n", version)
		os.Exit(0)
	}

	var err error
	pacificTZ, err = time.LoadLocation("America/Los_Angeles")
	if err != nil {
		slog.Error("Could not load Pacific timezone", "error", err)
		os.Exit(1)
	}

	// Load appropriate schedule based on route
	var weekdaySchedule, weekendSchedule []ScheduledTrip
	var terminusName string
	var terminusPolygon [][2]float64

	switch *routeFilter {
	case "4":
		weekdaySchedule, err = loadSchedule("schedules/route4-weekday.csv")
		if err != nil {
			slog.Error("Failed to load weekday schedule", "error", err)
			os.Exit(1)
		}
		weekendSchedule, err = loadSchedule("schedules/route4-weekend.csv")
		if err != nil {
			slog.Error("Failed to load weekend schedule", "error", err)
			os.Exit(1)
		}
		terminusName = "S Broadway"
		terminusPolygon = sBroadwayPolygon
	case "5":
		weekdaySchedule, err = loadSchedule("schedules/route5.csv")
		if err != nil {
			slog.Error("Failed to load schedule", "error", err)
			os.Exit(1)
		}
		weekendSchedule = weekdaySchedule // Route 5 uses same schedule
		terminusName = "Creekside"
		terminusPolygon = creeksidePolygon
	default:
		slog.Error("Unsupported route", "route", *routeFilter)
		os.Exit(1)
	}

	slog.Info("Loaded schedules",
		"weekday_trips", len(weekdaySchedule),
		"weekend_trips", len(weekendSchedule))

	// Load tracking data and detect arrivals
	arrivals, err := detectArrivals(*trackingFile, *routeFilter, terminusName, terminusPolygon)
	if err != nil {
		slog.Error("Failed to analyze tracking data", "error", err)
		os.Exit(1)
	}
	slog.Info("Detected arrivals", "count", len(arrivals))

	// Match arrivals to schedule and compute on-time stats
	matched := matchToSchedule(arrivals, weekdaySchedule, weekendSchedule, *onTimeWindow, terminusName)

	// Print results
	printResults(matched, *onTimeWindow, *routeFilter, terminusName)
}

func loadSchedule(filename string) ([]ScheduledTrip, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	var schedule []ScheduledTrip
	for i, record := range records {
		if i == 0 {
			continue // skip header
		}
		if len(record) < 3 {
			continue
		}
		schedule = append(schedule, ScheduledTrip{
			Direction: record[0],
			Stop:      record[1],
			Time:      record[2],
		})
	}
	return schedule, nil
}

func detectArrivals(filename, route, terminusName string, terminusPolygon [][2]float64) ([]ArrivalEvent, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Track when each vehicle was last seen at each location
	type vehicleState struct {
		lastLocation string
		lastTime     time.Time
		tripID       string
	}
	vehicleStates := make(map[string]*vehicleState)

	var arrivals []ArrivalEvent

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

		// Parse observation
		ts, err := time.Parse(time.RFC3339, fields[0])
		if err != nil {
			continue
		}
		ts = ts.In(pacificTZ)

		vehicleID := fields[3]
		obsRoute := fields[4]
		tripID := fields[5]
		lat, _ := strconv.ParseFloat(fields[6], 64)
		lon, _ := strconv.ParseFloat(fields[7], 64)

		// Filter by route
		if obsRoute != route {
			continue
		}

		// Determine location
		location := ""
		if isInPolygon(lat, lon, walnutCreekBARTPolygon) {
			location = "WC BART"
		} else if isInPolygon(lat, lon, terminusPolygon) {
			location = terminusName
		}

		// Get or create vehicle state
		state, exists := vehicleStates[vehicleID]
		if !exists {
			state = &vehicleState{}
			vehicleStates[vehicleID] = state
		}

		// Detect arrival: transition from non-terminus to terminus
		if location != "" && state.lastLocation != location {
			// This is an arrival event
			arrivals = append(arrivals, ArrivalEvent{
				Date:      ts.Format("2006-01-02"),
				Time:      ts,
				VehicleID: vehicleID,
				TripID:    tripID,
				Location:  location,
				Weekday:   ts.Weekday(),
			})
		}

		// Update state
		state.lastLocation = location
		state.lastTime = ts
		state.tripID = tripID
	}

	return arrivals, scanner.Err()
}

func matchToSchedule(arrivals []ArrivalEvent, weekdaySchedule, weekendSchedule []ScheduledTrip, windowMins int, terminusName string) []ArrivalEvent {
	// Build lookup of scheduled times by location for weekday and weekend
	weekdayBARTSchedule := []string{}
	weekdayTerminusSchedule := []string{}
	weekendBARTSchedule := []string{}
	weekendTerminusSchedule := []string{}

	for _, trip := range weekdaySchedule {
		if trip.Direction == "to_bart" && trip.Stop == "WC BART" {
			weekdayBARTSchedule = append(weekdayBARTSchedule, trip.Time)
		} else if trip.Stop == terminusName {
			weekdayTerminusSchedule = append(weekdayTerminusSchedule, trip.Time)
		}
	}

	for _, trip := range weekendSchedule {
		if trip.Direction == "to_bart" && trip.Stop == "WC BART" {
			weekendBARTSchedule = append(weekendBARTSchedule, trip.Time)
		} else if trip.Stop == terminusName {
			weekendTerminusSchedule = append(weekendTerminusSchedule, trip.Time)
		}
	}

	var matched []ArrivalEvent

	for _, arr := range arrivals {
		var schedTimes []string
		isWeekend := arr.Weekday == time.Saturday || arr.Weekday == time.Sunday

		if arr.Location == "WC BART" {
			if isWeekend {
				schedTimes = weekendBARTSchedule
			} else {
				schedTimes = weekdayBARTSchedule
			}
		} else if arr.Location == terminusName {
			if isWeekend {
				schedTimes = weekendTerminusSchedule
			} else {
				schedTimes = weekdayTerminusSchedule
			}
		} else {
			continue
		}

		if len(schedTimes) == 0 {
			continue
		}

		// Find closest scheduled time
		arrTime := arr.Time.Format("15:04")
		arrMins := timeToMinutes(arrTime)
		bestDelta := 9999
		bestSched := ""

		for _, schedTime := range schedTimes {
			schedMins := timeToMinutes(schedTime)
			delta := arrMins - schedMins

			// Handle day wraparound
			if delta > 720 {
				delta -= 1440
			} else if delta < -720 {
				delta += 1440
			}

			if abs(delta) < abs(bestDelta) {
				bestDelta = delta
				bestSched = schedTime
			}
		}

		// Only include if within reasonable matching window
		if abs(bestDelta) <= windowMins*3 {
			arr.Scheduled = bestSched
			arr.DeltaMins = bestDelta
			matched = append(matched, arr)
		}
	}

	return matched
}

func printResults(arrivals []ArrivalEvent, onTimeWindow int, route, terminusName string) {
	if len(arrivals) == 0 {
		fmt.Println("No arrival events detected.")
		return
	}

	// Group by date
	byDate := make(map[string][]ArrivalEvent)
	for _, arr := range arrivals {
		byDate[arr.Date] = append(byDate[arr.Date], arr)
	}

	// Sort dates
	dates := make([]string, 0, len(byDate))
	for date := range byDate {
		dates = append(dates, date)
	}
	sort.Strings(dates)

	// Overall stats
	totalArrivals := 0
	onTimeCount := 0
	earlyCount := 0
	lateCount := 0
	veryLateCount := 0 // > 10 mins
	totalDelay := 0

	wcBARTArrivals := 0
	wcBARTOnTime := 0
	terminusArrivals := 0
	terminusOnTime := 0

	// Weekday vs weekend
	weekdayArrivals := 0
	weekdayOnTime := 0
	weekendArrivals := 0
	weekendOnTime := 0

	fmt.Println("=" + strings.Repeat("=", 79))
	fmt.Printf("Route %s On-Time Performance Analysis\n", route)
	fmt.Printf("On-time window: %d minutes early to %d minutes late\n", onTimeWindow, onTimeWindow)
	fmt.Println("=" + strings.Repeat("=", 79))

	for _, date := range dates {
		dayArrivals := byDate[date]
		dayOnTime := 0
		dayTotal := len(dayArrivals)

		// Check if weekend
		if len(dayArrivals) > 0 {
			wd := dayArrivals[0].Weekday
			dayType := "weekday"
			if wd == time.Saturday || wd == time.Sunday {
				dayType = "weekend"
			}
			fmt.Printf("\n%s (%s, %d arrivals)\n", date, dayType, dayTotal)
		} else {
			fmt.Printf("\n%s (%d arrivals)\n", date, dayTotal)
		}
		fmt.Println(strings.Repeat("-", 60))

		for _, arr := range dayArrivals {
			totalArrivals++
			isWeekend := arr.Weekday == time.Saturday || arr.Weekday == time.Sunday

			status := ""
			isOnTime := arr.DeltaMins >= -onTimeWindow && arr.DeltaMins <= onTimeWindow

			if isOnTime {
				status = "ON TIME"
				onTimeCount++
				dayOnTime++
				if arr.Location == "WC BART" {
					wcBARTOnTime++
				} else {
					terminusOnTime++
				}
				if isWeekend {
					weekendOnTime++
				} else {
					weekdayOnTime++
				}
			} else if arr.DeltaMins < -onTimeWindow {
				status = "EARLY"
				earlyCount++
			} else {
				status = "LATE"
				lateCount++
				if arr.DeltaMins > 10 {
					veryLateCount++
				}
			}

			if arr.Location == "WC BART" {
				wcBARTArrivals++
			} else {
				terminusArrivals++
			}

			if isWeekend {
				weekendArrivals++
			} else {
				weekdayArrivals++
			}

			totalDelay += arr.DeltaMins

			deltaStr := fmt.Sprintf("%+d min", arr.DeltaMins)
			fmt.Printf("  %s  %-12s  sched %s  actual %s  %8s  %s\n",
				arr.Time.Format("15:04"),
				arr.Location,
				arr.Scheduled,
				arr.Time.Format("15:04"),
				deltaStr,
				status,
			)
		}

		if dayTotal > 0 {
			pct := float64(dayOnTime) / float64(dayTotal) * 100
			fmt.Printf("  Day on-time: %d/%d (%.1f%%)\n", dayOnTime, dayTotal, pct)
		}
	}

	// Summary
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("SUMMARY")
	fmt.Println(strings.Repeat("=", 80))

	onTimePct := float64(onTimeCount) / float64(totalArrivals) * 100
	avgDelay := float64(totalDelay) / float64(totalArrivals)

	fmt.Printf("\nTotal arrivals analyzed: %d\n", totalArrivals)
	fmt.Printf("Date range: %s to %s (%d days)\n", dates[0], dates[len(dates)-1], len(dates))

	fmt.Printf("\nOverall on-time performance:\n")
	fmt.Printf("  On time (within %d min): %d (%.1f%%)\n", onTimeWindow, onTimeCount, onTimePct)
	fmt.Printf("  Early (>%d min early):   %d (%.1f%%)\n", onTimeWindow, earlyCount, float64(earlyCount)/float64(totalArrivals)*100)
	fmt.Printf("  Late (>%d min late):     %d (%.1f%%)\n", onTimeWindow, lateCount, float64(lateCount)/float64(totalArrivals)*100)
	fmt.Printf("  Very late (>10 min):     %d (%.1f%%)\n", veryLateCount, float64(veryLateCount)/float64(totalArrivals)*100)
	fmt.Printf("  Average delay:           %.1f minutes\n", avgDelay)

	fmt.Printf("\nBy terminus:\n")
	if wcBARTArrivals > 0 {
		fmt.Printf("  WC BART:      %d/%d on time (%.1f%%)\n", wcBARTOnTime, wcBARTArrivals, float64(wcBARTOnTime)/float64(wcBARTArrivals)*100)
	}
	if terminusArrivals > 0 {
		fmt.Printf("  %s:  %d/%d on time (%.1f%%)\n", terminusName, terminusOnTime, terminusArrivals, float64(terminusOnTime)/float64(terminusArrivals)*100)
	}

	fmt.Printf("\nBy day type:\n")
	if weekdayArrivals > 0 {
		fmt.Printf("  Weekday:  %d/%d on time (%.1f%%)\n", weekdayOnTime, weekdayArrivals, float64(weekdayOnTime)/float64(weekdayArrivals)*100)
	}
	if weekendArrivals > 0 {
		fmt.Printf("  Weekend:  %d/%d on time (%.1f%%)\n", weekendOnTime, weekendArrivals, float64(weekendOnTime)/float64(weekendArrivals)*100)
	}
}

// isInPolygon checks if a point is inside a polygon using ray casting
func isInPolygon(lat, lon float64, polygon [][2]float64) bool {
	n := len(polygon)
	inside := false

	j := n - 1
	for i := 0; i < n; i++ {
		yi, xi := polygon[i][0], polygon[i][1]
		yj, xj := polygon[j][0], polygon[j][1]

		if ((yi > lat) != (yj > lat)) &&
			(lon < (xj-xi)*(lat-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

func timeToMinutes(t string) int {
	parts := strings.Split(t, ":")
	if len(parts) != 2 {
		return 0
	}
	hours, _ := strconv.Atoi(parts[0])
	mins, _ := strconv.Atoi(parts[1])
	return hours*60 + mins
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
