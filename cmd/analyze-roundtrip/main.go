// analyze-roundtrip measures round trip times from BART departure to BART return.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	trackingFile = flag.String("tracking-file", "vehicle-tracking.tsv", "Vehicle tracking TSV file")
	route        = flag.String("route", "5", "Route to analyze")
	threshold    = flag.Int("threshold", 25, "Threshold in minutes")
)

var walnutCreekBARTPolygon = [][2]float64{
	{37.90702176601014, -122.06856813397798},
	{37.90631565035961, -122.06699383073966},
	{37.90442920597368, -122.06813046461296},
	{37.90528481242584, -122.06935855178641},
}

func isInPolygon(lat, lon float64, polygon [][2]float64) bool {
	n := len(polygon)
	inside := false
	j := n - 1
	for i := 0; i < n; i++ {
		yi, xi := polygon[i][0], polygon[i][1]
		yj, xj := polygon[j][0], polygon[j][1]
		if ((yi > lat) != (yj > lat)) && (lon < (xj-xi)*(lat-yi)/(yj-yi)+xi) {
			inside = !inside
		}
		j = i
	}
	return inside
}

type Position struct {
	Time    time.Time
	Vehicle string
	AtBART  bool
}

type RoundTrip struct {
	Vehicle   string
	Departure time.Time
	Arrival   time.Time
	Duration  int // minutes
}

func main() {
	flag.Parse()

	file, err := os.Open(*trackingFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening file: %v\n", err)
		os.Exit(1)
	}
	defer file.Close()

	pacificTZ, _ := time.LoadLocation("America/Los_Angeles")

	// Track state for each vehicle
	type vehicleState struct {
		wasAtBART     bool
		departureTime time.Time
	}
	states := make(map[string]*vehicleState)

	var roundTrips []RoundTrip

	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum == 1 {
			continue
		}

		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 10 {
			continue
		}

		ts, _ := time.Parse(time.RFC3339, fields[0])
		ts = ts.In(pacificTZ)
		vehicle := fields[3]
		obsRoute := fields[4]
		lat, _ := strconv.ParseFloat(fields[6], 64)
		lon, _ := strconv.ParseFloat(fields[7], 64)

		if obsRoute != *route {
			continue
		}

		atBART := isInPolygon(lat, lon, walnutCreekBARTPolygon)

		state := states[vehicle]
		if state == nil {
			state = &vehicleState{wasAtBART: atBART}
			states[vehicle] = state
			continue
		}

		// Detect departure from BART (was at BART, now not)
		if state.wasAtBART && !atBART {
			state.departureTime = ts
		}

		// Detect return to BART (was not at BART, now at BART)
		if !state.wasAtBART && atBART && !state.departureTime.IsZero() {
			// Must be same day
			if state.departureTime.Format("2006-01-02") == ts.Format("2006-01-02") {
				duration := int(ts.Sub(state.departureTime).Minutes())
				// Filter reasonable round trips (10-90 minutes)
				if duration >= 10 && duration <= 90 {
					roundTrips = append(roundTrips, RoundTrip{
						Vehicle:   vehicle,
						Departure: state.departureTime,
						Arrival:   ts,
						Duration:  duration,
					})
				}
			}
			state.departureTime = time.Time{} // reset
		}

		state.wasAtBART = atBART
	}

	// Analyze results
	var durations []int
	for _, rt := range roundTrips {
		durations = append(durations, rt.Duration)
	}
	sort.Ints(durations)

	underThreshold := 0
	for _, d := range durations {
		if d < *threshold {
			underThreshold++
		}
	}

	fmt.Printf("Route %s Round Trip Analysis (BART → BART)\n", *route)
	fmt.Printf("==========================================\n\n")
	fmt.Printf("Total round trips measured: %d\n\n", len(durations))

	if len(durations) == 0 {
		fmt.Println("No round trips detected.")
		return
	}

	fmt.Printf("Trips under %d minutes: %d (%.1f%%)\n", *threshold, underThreshold, float64(underThreshold)*100/float64(len(durations)))
	fmt.Printf("Trips %d+ minutes:      %d (%.1f%%)\n\n", *threshold, len(durations)-underThreshold, float64(len(durations)-underThreshold)*100/float64(len(durations)))

	fmt.Printf("Round trip times:\n")
	fmt.Printf("  Min:    %d min\n", durations[0])
	fmt.Printf("  Median: %d min\n", durations[len(durations)/2])
	fmt.Printf("  Max:    %d min\n\n", durations[len(durations)-1])

	// Percentiles
	p25 := durations[len(durations)*25/100]
	p75 := durations[len(durations)*75/100]
	p90 := durations[len(durations)*90/100]
	fmt.Printf("Percentiles:\n")
	fmt.Printf("  25th: %d min\n", p25)
	fmt.Printf("  50th: %d min (median)\n", durations[len(durations)/2])
	fmt.Printf("  75th: %d min\n", p75)
	fmt.Printf("  90th: %d min\n\n", p90)

	// Distribution
	fmt.Printf("Round trip distribution:\n")
	buckets := make(map[int]int)
	for _, d := range durations {
		buckets[d]++
	}
	maxCount := 0
	for _, c := range buckets {
		if c > maxCount {
			maxCount = c
		}
	}
	scale := 1
	if maxCount > 60 {
		scale = maxCount / 60
	}
	for t := 10; t <= 60; t++ {
		if buckets[t] > 0 {
			bar := strings.Repeat("█", buckets[t]/scale)
			fmt.Printf("  %2d min: %3d %s\n", t, buckets[t], bar)
		}
	}

	// Show some example trips
	fmt.Printf("\nSample round trips (first 10):\n")
	for i, rt := range roundTrips {
		if i >= 10 {
			break
		}
		fmt.Printf("  %s: Bus %s departed %s, returned %s (%d min)\n",
			rt.Departure.Format("2006-01-02"),
			rt.Vehicle,
			rt.Departure.Format("15:04"),
			rt.Arrival.Format("15:04"),
			rt.Duration)
	}
}
