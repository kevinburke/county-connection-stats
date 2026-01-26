// analyze-triptime measures actual trip times from tracking data.
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
	threshold    = flag.Int("threshold", 25, "Threshold in minutes for under/over comparison")
)

var walnutCreekBARTPolygon = [][2]float64{
	{37.90702176601014, -122.06856813397798},
	{37.90631565035961, -122.06699383073966},
	{37.90442920597368, -122.06813046461296},
	{37.90528481242584, -122.06935855178641},
}

var creeksidePolygon = [][2]float64{
	{37.88260858311487, -122.05108337231785},
	{37.88286182615337, -122.04946921154641},
	{37.88169300486415, -122.04886698642677},
	{37.88158391393072, -122.05094022044516},
}

var sBroadwayPolygon = [][2]float64{
	{37.8979313200401, -122.05875803463786},
	{37.89797453351041, -122.05617324154504},
	{37.896094724101275, -122.055806332356},
	{37.8967602253355, -122.05894970361719},
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

type Arrival struct {
	Time     time.Time
	Location string
	Vehicle  string
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

	// Determine terminus based on route
	var terminusPolygon [][2]float64
	var terminusName string
	switch *route {
	case "5":
		terminusPolygon = creeksidePolygon
		terminusName = "Creekside"
	case "4":
		terminusPolygon = sBroadwayPolygon
		terminusName = "S Broadway"
	default:
		fmt.Fprintf(os.Stderr, "Unknown route: %s\n", *route)
		os.Exit(1)
	}

	// Track arrivals by vehicle
	vehicleArrivals := make(map[string][]Arrival)
	lastLocation := make(map[string]string)

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

		loc := ""
		if isInPolygon(lat, lon, walnutCreekBARTPolygon) {
			loc = "BART"
		} else if isInPolygon(lat, lon, terminusPolygon) {
			loc = terminusName
		}

		if loc != "" && lastLocation[vehicle] != loc {
			vehicleArrivals[vehicle] = append(vehicleArrivals[vehicle], Arrival{ts, loc, vehicle})
		}
		lastLocation[vehicle] = loc
	}

	// Calculate trip times
	var tripTimes []int
	var bartToTerminus []int
	var terminusToBART []int

	for _, arrivals := range vehicleArrivals {
		for i := 1; i < len(arrivals); i++ {
			prev := arrivals[i-1]
			curr := arrivals[i]

			// Must be different locations and same day
			if prev.Location == curr.Location {
				continue
			}
			if prev.Time.Format("2006-01-02") != curr.Time.Format("2006-01-02") {
				continue
			}

			mins := int(curr.Time.Sub(prev.Time).Minutes())
			if mins < 5 || mins > 60 {
				continue
			} // filter outliers

			tripTimes = append(tripTimes, mins)
			if prev.Location == "BART" {
				bartToTerminus = append(bartToTerminus, mins)
			} else {
				terminusToBART = append(terminusToBART, mins)
			}
		}
	}

	sort.Ints(tripTimes)
	sort.Ints(bartToTerminus)
	sort.Ints(terminusToBART)

	underThreshold := 0
	for _, t := range tripTimes {
		if t < *threshold {
			underThreshold++
		}
	}

	fmt.Printf("Route %s Trip Time Analysis\n", *route)
	fmt.Printf("===========================\n\n")
	fmt.Printf("Total trips measured: %d\n\n", len(tripTimes))

	fmt.Printf("Trips under %d minutes: %d (%.1f%%)\n", *threshold, underThreshold, float64(underThreshold)*100/float64(len(tripTimes)))
	fmt.Printf("Trips %d+ minutes:      %d (%.1f%%)\n\n", *threshold, len(tripTimes)-underThreshold, float64(len(tripTimes)-underThreshold)*100/float64(len(tripTimes)))

	if len(tripTimes) > 0 {
		fmt.Printf("Overall trip times:\n")
		fmt.Printf("  Min:    %d min\n", tripTimes[0])
		fmt.Printf("  Median: %d min\n", tripTimes[len(tripTimes)/2])
		fmt.Printf("  Max:    %d min\n\n", tripTimes[len(tripTimes)-1])
	}

	if len(bartToTerminus) > 0 {
		under := 0
		for _, t := range bartToTerminus {
			if t < *threshold {
				under++
			}
		}
		fmt.Printf("BART -> %s: %d trips\n", terminusName, len(bartToTerminus))
		fmt.Printf("  Under %d min: %d (%.1f%%)\n", *threshold, under, float64(under)*100/float64(len(bartToTerminus)))
		fmt.Printf("  Min: %d, Median: %d, Max: %d\n\n", bartToTerminus[0], bartToTerminus[len(bartToTerminus)/2], bartToTerminus[len(bartToTerminus)-1])
	}

	if len(terminusToBART) > 0 {
		under := 0
		for _, t := range terminusToBART {
			if t < *threshold {
				under++
			}
		}
		fmt.Printf("%s -> BART: %d trips\n", terminusName, len(terminusToBART))
		fmt.Printf("  Under %d min: %d (%.1f%%)\n", *threshold, under, float64(under)*100/float64(len(terminusToBART)))
		fmt.Printf("  Min: %d, Median: %d, Max: %d\n\n", terminusToBART[0], terminusToBART[len(terminusToBART)/2], terminusToBART[len(terminusToBART)-1])
	}

	// Distribution
	fmt.Printf("Trip time distribution:\n")
	buckets := make(map[int]int)
	for _, t := range tripTimes {
		buckets[t]++
	}
	for t := 5; t <= 40; t++ {
		if buckets[t] > 0 {
			bar := strings.Repeat("█", buckets[t]/3)
			fmt.Printf("  %2d min: %3d %s\n", t, buckets[t], bar)
		}
	}
}
