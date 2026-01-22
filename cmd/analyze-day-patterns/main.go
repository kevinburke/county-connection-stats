// analyze-day-patterns analyzes temporal patterns in bus operations.
//
// This tool examines when different types of buses (BEBs vs diesels) operate,
// looking for patterns like:
//   - Which days of the week do BEBs run?
//   - Do diesels only run on weekends?
//   - Are there monthly or seasonal patterns?
//
// Usage:
//
//	go run ./cmd/analyze-day-patterns -file var/extracted/cc45-stop-observations.txt
//
// Options:
//
//	-file      Path to extracted stop_observations file (default: var/extracted/cc45-stop-observations.txt)
//	-version   Print version and exit
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
	"time"
)

const (
	analyzeVersion   = "0.1.0"
	minDieselTrips   = 10
	defaultExtracted = "var/extracted/cc45-stop-observations.txt"
)

var (
	versionFlag = flag.Bool("version", false, "Print version and exit")
	fileFlag    = flag.String("file", defaultExtracted, "Extracted stop_observations file to analyze")

	bebSet = map[string]struct{}{
		"1600": {}, "1601": {}, "1602": {}, "1603": {},
		"1800": {}, "1801": {}, "1802": {}, "1803": {},
	}
)

type BusType string

const (
	BusTypeBEB    BusType = "BEB"
	BusTypeDiesel BusType = "diesel"
)

type DayStats struct {
	Date        string
	Weekday     time.Weekday
	Trips       int
	Vehicles    map[string]struct{} // unique vehicles that ran
	BEBTrips    int
	DieselTrips int
}

type VehicleDayStats struct {
	VehicleID string
	Type      BusType
	Weekdays  map[time.Weekday]int // trips by day of week
	Weekends  int                  // weekend trips
	Weekday   int                  // weekday trips
}

func main() {
	flag.Parse()

	if *versionFlag {
		fmt.Printf("analyze-day-patterns version %s\n", analyzeVersion)
		os.Exit(0)
	}

	if _, err := os.Stat(*fileFlag); os.IsNotExist(err) {
		slog.Error("File does not exist", "file", *fileFlag)
		os.Exit(1)
	}

	// Read and process the data
	dayStats, vehicleStats, err := processFile(*fileFlag)
	if err != nil {
		slog.Error("Failed to process file", "error", err)
		os.Exit(1)
	}

	// Generate reports
	printDayOfWeekReport(dayStats, vehicleStats)
	printVehiclePatterns(vehicleStats)
	printWeekdayWeekendSummary(dayStats)
	printWeekendBEBs(dayStats)
}

func processFile(path string) (map[string]*DayStats, map[string]*VehicleDayStats, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, nil, err
	}

	cols := make(map[string]int)
	for i, col := range header {
		cols[col] = i
	}

	required := []string{"trip_id", "service_date", "vehicle_id"}
	for _, c := range required {
		if _, ok := cols[c]; !ok {
			return nil, nil, fmt.Errorf("missing column: %s", c)
		}
	}

	dayStats := make(map[string]*DayStats)
	vehicleStats := make(map[string]*VehicleDayStats)
	seenTrips := make(map[string]string) // tripKey -> vehicleID

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
		if _, exists := seenTrips[tripKey]; exists {
			continue
		}
		seenTrips[tripKey] = vehicleID

		// Parse service date (YYYYMMDD format)
		date, err := time.Parse("20060102", serviceDate)
		if err != nil {
			continue
		}

		busType := classifyBus(vehicleID)
		weekday := date.Weekday()

		// Update day stats
		ds := dayStats[serviceDate]
		if ds == nil {
			ds = &DayStats{
				Date:     serviceDate,
				Weekday:  weekday,
				Vehicles: make(map[string]struct{}),
			}
			dayStats[serviceDate] = ds
		}
		ds.Trips++
		ds.Vehicles[vehicleID] = struct{}{}
		if busType == BusTypeBEB {
			ds.BEBTrips++
		} else {
			ds.DieselTrips++
		}

		// Update vehicle stats
		vs := vehicleStats[vehicleID]
		if vs == nil {
			vs = &VehicleDayStats{
				VehicleID: vehicleID,
				Type:      busType,
				Weekdays:  make(map[time.Weekday]int),
			}
			vehicleStats[vehicleID] = vs
		}
		vs.Weekdays[weekday]++
		if weekday == time.Saturday || weekday == time.Sunday {
			vs.Weekends++
		} else {
			vs.Weekday++
		}
	}

	slog.Info("File processed", "file", path, "records", lines, "unique_trips", len(seenTrips), "unique_days", len(dayStats))

	// Filter out diesel buses with too few trips
	for id, stats := range vehicleStats {
		if stats.Type == BusTypeDiesel {
			totalTrips := stats.Weekday + stats.Weekends
			if totalTrips < minDieselTrips {
				delete(vehicleStats, id)
			}
		}
	}

	return dayStats, vehicleStats, nil
}

func printDayOfWeekReport(dayStats map[string]*DayStats, vehicleStats map[string]*VehicleDayStats) {
	// Aggregate by day of week
	dowStats := make(map[time.Weekday]struct {
		Days        int
		TotalTrips  int
		BEBTrips    int
		DieselTrips int
		Vehicles    map[string]struct{}
	})

	for i := time.Sunday; i <= time.Saturday; i++ {
		dowStats[i] = struct {
			Days        int
			TotalTrips  int
			BEBTrips    int
			DieselTrips int
			Vehicles    map[string]struct{}
		}{
			Vehicles: make(map[string]struct{}),
		}
	}

	for _, ds := range dayStats {
		stat := dowStats[ds.Weekday]
		stat.Days++
		stat.TotalTrips += ds.Trips
		stat.BEBTrips += ds.BEBTrips
		stat.DieselTrips += ds.DieselTrips
		for v := range ds.Vehicles {
			stat.Vehicles[v] = struct{}{}
		}
		dowStats[ds.Weekday] = stat
	}

	fmt.Println("\n=== Day of Week Analysis ===")
	fmt.Println("Day       | Days | Avg Trips | BEB Trips | Diesel Trips | Unique Vehicles")
	fmt.Println("----------|------|-----------|-----------|--------------|----------------")

	days := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
	for _, dow := range days {
		stat := dowStats[dow]
		avgTrips := 0.0
		if stat.Days > 0 {
			avgTrips = float64(stat.TotalTrips) / float64(stat.Days)
		}
		fmt.Printf("%-9s | %4d | %9.1f | %9d | %12d | %15d\n",
			dow, stat.Days, avgTrips, stat.BEBTrips, stat.DieselTrips, len(stat.Vehicles))
	}
}

func printVehiclePatterns(vehicleStats map[string]*VehicleDayStats) {
	// Separate BEBs and diesels
	var bebs, diesels []*VehicleDayStats
	for _, vs := range vehicleStats {
		if vs.Type == BusTypeBEB {
			bebs = append(bebs, vs)
		} else {
			diesels = append(diesels, vs)
		}
	}

	sort.Slice(bebs, func(i, j int) bool {
		return bebs[i].VehicleID < bebs[j].VehicleID
	})
	sort.Slice(diesels, func(i, j int) bool {
		return diesels[i].VehicleID < diesels[j].VehicleID
	})

	fmt.Println("\n\n=== Battery Electric Bus Patterns ===")
	printVehicleList(bebs)

	fmt.Println("\n\n=== Diesel Bus Patterns ===")
	printVehicleList(diesels)
}

func printVehicleList(vehicles []*VehicleDayStats) {
	if len(vehicles) == 0 {
		fmt.Println("(no vehicles)")
		return
	}

	fmt.Println("Vehicle | Mon | Tue | Wed | Thu | Fri | Sat | Sun | Weekday% | Weekend%")
	fmt.Println("--------|-----|-----|-----|-----|-----|-----|-----|----------|----------")

	for _, vs := range vehicles {
		total := vs.Weekday + vs.Weekends
		weekdayPct := 0.0
		weekendPct := 0.0
		if total > 0 {
			weekdayPct = float64(vs.Weekday) * 100 / float64(total)
			weekendPct = float64(vs.Weekends) * 100 / float64(total)
		}

		fmt.Printf("%-7s | %3d | %3d | %3d | %3d | %3d | %3d | %3d | %7.1f%% | %7.1f%%\n",
			vs.VehicleID,
			vs.Weekdays[time.Monday],
			vs.Weekdays[time.Tuesday],
			vs.Weekdays[time.Wednesday],
			vs.Weekdays[time.Thursday],
			vs.Weekdays[time.Friday],
			vs.Weekdays[time.Saturday],
			vs.Weekdays[time.Sunday],
			weekdayPct,
			weekendPct,
		)
	}
}

func printWeekdayWeekendSummary(dayStats map[string]*DayStats) {
	var weekdayDays, weekendDays int
	var weekdayTrips, weekendTrips int
	var weekdayBEB, weekendBEB int
	var weekdayDiesel, weekendDiesel int

	for _, ds := range dayStats {
		if ds.Weekday == time.Saturday || ds.Weekday == time.Sunday {
			weekendDays++
			weekendTrips += ds.Trips
			weekendBEB += ds.BEBTrips
			weekendDiesel += ds.DieselTrips
		} else {
			weekdayDays++
			weekdayTrips += ds.Trips
			weekdayBEB += ds.BEBTrips
			weekdayDiesel += ds.DieselTrips
		}
	}

	fmt.Println("\n\n=== Weekday vs Weekend Summary ===")
	fmt.Printf("Period   | Days | Avg Trips/Day | Total BEB | Total Diesel | BEB %% | Diesel %%\n")
	fmt.Println("---------|------|---------------|-----------|--------------|--------|----------")

	if weekdayDays > 0 {
		avgWeekday := float64(weekdayTrips) / float64(weekdayDays)
		bebPct := float64(weekdayBEB) * 100 / float64(weekdayTrips)
		dieselPct := float64(weekdayDiesel) * 100 / float64(weekdayTrips)
		fmt.Printf("Weekday  | %4d | %13.1f | %9d | %12d | %5.1f%% | %7.1f%%\n",
			weekdayDays, avgWeekday, weekdayBEB, weekdayDiesel, bebPct, dieselPct)
	}

	if weekendDays > 0 {
		avgWeekend := float64(weekendTrips) / float64(weekendDays)
		bebPct := float64(weekendBEB) * 100 / float64(weekendTrips)
		dieselPct := float64(weekendDiesel) * 100 / float64(weekendTrips)
		fmt.Printf("Weekend  | %4d | %13.1f | %9d | %12d | %5.1f%% | %7.1f%%\n",
			weekendDays, avgWeekend, weekendBEB, weekendDiesel, bebPct, dieselPct)
	}
}

func classifyBus(vehicleID string) BusType {
	if _, ok := bebSet[vehicleID]; ok {
		return BusTypeBEB
	}
	return BusTypeDiesel
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

func formatDayList(days []string) string {
	if len(days) == 0 {
		return "(none)"
	}
	if len(days) <= 5 {
		return strings.Join(days, ", ")
	}
	return fmt.Sprintf("%s, ... (%d more)", strings.Join(days[:5], ", "), len(days)-5)
}

type WeekendBEBRecord struct {
	Date      string
	Weekday   time.Weekday
	VehicleID string
	Trips     int
}

func printWeekendBEBs(dayStats map[string]*DayStats) {
	fmt.Println("\n\n=== BEB Weekend Service (All Instances) ===")
	fmt.Println("Date       | Day | Vehicle | Trips")
	fmt.Println("-----------|-----|---------|------")

	// We need to re-process to get this data
	// For now, let's scan the file again specifically for weekend BEB trips
	records, err := getWeekendBEBRecords(*fileFlag)
	if err != nil {
		slog.Error("Failed to get weekend BEB records", "error", err)
		return
	}

	if len(records) == 0 {
		fmt.Println("(no BEB weekend service found)")
		return
	}

	// Sort by date, then vehicle
	sort.Slice(records, func(i, j int) bool {
		if records[i].Date != records[j].Date {
			return records[i].Date < records[j].Date
		}
		return records[i].VehicleID < records[j].VehicleID
	})

	for _, rec := range records {
		fmt.Printf("%s | %3s | %7s | %5d\n",
			formatDate(rec.Date), rec.Weekday.String()[:3], rec.VehicleID, rec.Trips)
	}

	fmt.Printf("\nTotal weekend BEB service days: %d\n", len(records))
}

func getWeekendBEBRecords(path string) ([]WeekendBEBRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, err
	}

	cols := make(map[string]int)
	for i, col := range header {
		cols[col] = i
	}

	// Track trips per vehicle per day
	vehicleDayTrips := make(map[string]map[string]int) // vehicleID -> date -> trip count
	seenTrips := make(map[string]bool)                 // tripKey -> seen

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

		// Only track BEBs
		if classifyBus(vehicleID) != BusTypeBEB {
			continue
		}

		serviceDate := valueAt(record, cols["service_date"])
		tripID := valueAt(record, cols["trip_id"])
		if serviceDate == "" || tripID == "" {
			continue
		}

		// Parse service date to check if weekend
		date, err := time.Parse("20060102", serviceDate)
		if err != nil {
			continue
		}

		if date.Weekday() != time.Saturday && date.Weekday() != time.Sunday {
			continue
		}

		tripKey := serviceDate + "|" + tripID
		if seenTrips[tripKey] {
			continue
		}
		seenTrips[tripKey] = true

		if vehicleDayTrips[vehicleID] == nil {
			vehicleDayTrips[vehicleID] = make(map[string]int)
		}
		vehicleDayTrips[vehicleID][serviceDate]++
	}

	// Convert to records
	var records []WeekendBEBRecord
	for vehicleID, dayTrips := range vehicleDayTrips {
		for dateStr, trips := range dayTrips {
			date, _ := time.Parse("20060102", dateStr)
			records = append(records, WeekendBEBRecord{
				Date:      dateStr,
				Weekday:   date.Weekday(),
				VehicleID: vehicleID,
				Trips:     trips,
			})
		}
	}

	return records, nil
}

func formatDate(dateStr string) string {
	if len(dateStr) != 8 {
		return dateStr
	}
	// Convert YYYYMMDD to YYYY-MM-DD
	return dateStr[:4] + "-" + dateStr[4:6] + "-" + dateStr[6:]
}
