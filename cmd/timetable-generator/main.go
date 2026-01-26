// timetable-generator creates hypothetical timetables with different headways.
//
// It shows what the Route 5 schedule could look like if buses ran more frequently,
// for example every 40 minutes instead of every 45 minutes.
//
// The generator accounts for driver rest time at BART and calculates how many
// buses are needed to maintain the schedule.
//
// Usage:
//
//	go run ./cmd/timetable-generator
//	go run ./cmd/timetable-generator -headway 40
//	go run ./cmd/timetable-generator -headway 30 -rest-time 5
//
// Options:
//
//	-headway         Desired headway in minutes (default: 40)
//	-first-departure First departure time from WC BART (default: 05:53)
//	-last-departure  Latest departure time from WC BART (default: 19:23)
//	-trip-time       One-way trip time in minutes (default: 14, based on observed data)
//	-rest-time       Minimum rest time at BART in minutes (default: 5)
//	-compare         Show comparison with current 45-min schedule
//	-version         Print version and exit
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

const version = "0.1.0"

var (
	headway        = flag.Int("headway", 40, "Desired headway in minutes")
	firstDeparture = flag.String("first-departure", "05:53", "First departure from WC BART (HH:MM)")
	lastDeparture  = flag.String("last-departure", "19:23", "Latest departure from WC BART (HH:MM)")
	tripTime       = flag.Int("trip-time", 14, "One-way trip time in minutes (observed median is ~14)")
	restTime       = flag.Int("rest-time", 5, "Minimum rest time at BART in minutes")
	compare        = flag.Bool("compare", true, "Show comparison with current 45-min schedule")
	versionFlag    = flag.Bool("version", false, "Print version and exit")
)

func main() {
	flag.Parse()

	if *versionFlag {
		fmt.Printf("timetable-generator version %s\n", version)
		os.Exit(0)
	}

	firstMins := parseTime(*firstDeparture)
	lastMins := parseTime(*lastDeparture)
	serviceSpan := lastMins - firstMins
	roundTripTime := *tripTime * 2
	minCycleTime := roundTripTime + *restTime

	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("Route 5 Timetable Generator\n")
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("\nService span: %s to %s (%d hours %d minutes)\n",
		*firstDeparture, *lastDeparture,
		serviceSpan/60, serviceSpan%60)
	fmt.Printf("One-way trip time: %d minutes\n", *tripTime)
	fmt.Printf("Round trip time: %d minutes\n", roundTripTime)
	fmt.Printf("Minimum rest at BART: %d minutes\n", *restTime)
	fmt.Printf("Minimum cycle time (round trip + rest): %d minutes\n", minCycleTime)

	if *compare {
		fmt.Println("\n" + strings.Repeat("-", 80))
		fmt.Println("CURRENT SCHEDULE (45-minute headway)")
		fmt.Println(strings.Repeat("-", 80))
		currentTrips := generateTimetable(firstMins, lastMins, 45)
		printTimetable(currentTrips, *tripTime)
		currentBuses := busesNeeded(45, minCycleTime)
		currentRest := 45 - roundTripTime
		fmt.Printf("\nTotal one-way trips: %d (%d round trips)\n",
			len(currentTrips)*2, len(currentTrips))
		fmt.Printf("Buses needed: %d\n", currentBuses)
		fmt.Printf("Actual rest time at BART: %d minutes\n", currentRest)
	}

	fmt.Println("\n" + strings.Repeat("-", 80))
	fmt.Printf("PROPOSED SCHEDULE (%d-minute headway)\n", *headway)
	fmt.Println(strings.Repeat("-", 80))
	proposedTrips := generateTimetable(firstMins, lastMins, *headway)
	printTimetable(proposedTrips, *tripTime)
	proposedBuses := busesNeeded(*headway, minCycleTime)
	// With multiple buses, each bus runs every (headway * buses) minutes
	// Rest time = (headway * buses) - roundTripTime
	proposedRest := (*headway * proposedBuses) - roundTripTime
	fmt.Printf("\nTotal one-way trips: %d (%d round trips)\n",
		len(proposedTrips)*2, len(proposedTrips))
	fmt.Printf("Buses needed: %d\n", proposedBuses)
	fmt.Printf("Actual rest time at BART: %d minutes (each bus cycles every %d min)\n",
		proposedRest, *headway*proposedBuses)

	if *compare {
		currentTrips := generateTimetable(firstMins, lastMins, 45)
		currentBuses := busesNeeded(45, minCycleTime)
		extraTrips := len(proposedTrips) - len(currentTrips)
		extraBuses := proposedBuses - currentBuses
		fmt.Println("\n" + strings.Repeat("=", 80))
		fmt.Println("COMPARISON")
		fmt.Println(strings.Repeat("=", 80))
		fmt.Printf("\nCurrent (45-min headway):  %d departures, %d bus(es)\n", len(currentTrips), currentBuses)
		fmt.Printf("Proposed (%d-min headway): %d departures, %d bus(es)\n", *headway, len(proposedTrips), proposedBuses)
		fmt.Printf("\nDifference: %+d departures per direction\n", extraTrips)
		fmt.Printf("            %+d total one-way trips per day\n", extraTrips*2)
		if extraBuses != 0 {
			fmt.Printf("            %+d bus(es) required\n", extraBuses)
		}

		if extraTrips > 0 {
			// Calculate percentage improvement
			pctImprovement := float64(extraTrips) / float64(len(currentTrips)) * 100
			fmt.Printf("\nService frequency improvement: %.1f%%\n", pctImprovement)
			fmt.Printf("\nWith %d-minute headways, riders would wait an average of %d minutes\n",
				*headway, *headway/2)
			fmt.Printf("instead of 22-23 minutes (with 45-minute headways).\n")
		}

		// Show what this means for wait times
		fmt.Println("\n" + strings.Repeat("-", 80))
		fmt.Println("IMPACT ON RIDERS")
		fmt.Println(strings.Repeat("-", 80))
		fmt.Printf("\nAverage wait time (random arrival):\n")
		fmt.Printf("  Current (45-min): ~22.5 minutes\n")
		fmt.Printf("  Proposed (%d-min): ~%.1f minutes\n", *headway, float64(*headway)/2)
		fmt.Printf("  Time saved per trip: ~%.1f minutes\n", 22.5-float64(*headway)/2)
	}
}

// busesNeeded calculates the number of buses required to maintain a headway
// given the minimum cycle time (round trip + rest).
// If headway >= minCycleTime, one bus can handle it.
// Otherwise, multiple buses are needed so each bus has time to complete its cycle.
func busesNeeded(headway, minCycleTime int) int {
	if headway >= minCycleTime {
		return 1
	}
	// Number of buses = ceil(minCycleTime / headway)
	buses := minCycleTime / headway
	if minCycleTime%headway > 0 {
		buses++
	}
	return buses
}

// generateTimetable generates departure times from WC BART
func generateTimetable(firstMins, lastMins, headway int) []int {
	var departures []int
	for t := firstMins; t <= lastMins; t += headway {
		departures = append(departures, t)
	}
	return departures
}

func printTimetable(bartDepartures []int, tripTime int) {
	fmt.Println("\n  WC BART      Creekside     Creekside     WC BART")
	fmt.Println("  Departure    Arrival       Departure     Arrival")
	fmt.Println("  ---------    ---------     ---------     -------")

	for _, dep := range bartDepartures {
		creeksideArr := dep + tripTime
		creeksideDep := creeksideArr // immediate turnaround for display
		bartReturn := creeksideDep + tripTime

		fmt.Printf("  %s         %s          %s          %s\n",
			formatTime(dep),
			formatTime(creeksideArr),
			formatTime(creeksideDep),
			formatTime(bartReturn))
	}
}

func parseTime(t string) int {
	parts := strings.Split(t, ":")
	if len(parts) != 2 {
		return 0
	}
	hours := 0
	mins := 0
	fmt.Sscanf(parts[0], "%d", &hours)
	fmt.Sscanf(parts[1], "%d", &mins)
	return hours*60 + mins
}

func formatTime(mins int) string {
	h := mins / 60
	m := mins % 60
	suffix := "am"
	displayH := h
	if h >= 12 {
		suffix = "pm"
		if h > 12 {
			displayH = h - 12
		}
	}
	if h == 0 {
		displayH = 12
	}
	return fmt.Sprintf("%d:%02d%s", displayH, m, suffix)
}
