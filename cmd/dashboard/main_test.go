package main

import (
	"bytes"
	"html/template"
	"testing"
	"time"
)

func TestTemplateRenders(t *testing.T) {
	// Initialize timezone for template functions
	var err error
	pacificTZ, err = time.LoadLocation("America/Los_Angeles")
	if err != nil {
		pacificTZ = time.UTC
	}

	tmpl, err := template.New("dashboard").Funcs(template.FuncMap{
		"formatTime": func(t time.Time) string {
			return t.Format("2006-01-02 3:04 PM MST")
		},
		"formatTimeHuman": func(t time.Time) string {
			return "just now"
		},
		"formatPct": func(f float64) string {
			return "0.0%"
		},
		"formatFloat": func(f float64) string {
			return "0.0"
		},
		"formatCoord": func(f float64) string {
			return "0.00000"
		},
		"mapsURL": func(lat, lon float64) string {
			return "https://maps.google.com"
		},
		"gtZero": func(f float64) bool {
			return f > 0
		},
		"formatHours": func(f float64) string {
			return "0"
		},
		"formatMinPerDay": func(f float64) string {
			return "0 min/day"
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
			return date
		},
		"busTypeClass": func(t string) string {
			if t == "BEB" {
				return "beb"
			}
			return "diesel"
		},
		"isAtWalnutCreekBART": func(lat, lon float64) bool {
			return false
		},
		"walnutCreekBARTURL": func() string {
			return "https://maps.google.com"
		},
	}).Parse(htmlTemplate)
	if err != nil {
		t.Fatalf("failed to parse template: %v", err)
	}

	// Test with minimal data
	data := DashboardData{
		GeneratedAt:    time.Now(),
		Routes:         "4, 5",
		TotalValidDays: 100,
		DataStartDate:  "2024-01-01",
		DataEndDate:    "2024-04-10",
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("failed to execute template with minimal data: %v", err)
	}

	if buf.Len() == 0 {
		t.Error("template produced empty output")
	}
}

func TestTemplateRendersWithBEBDrought(t *testing.T) {
	var err error
	pacificTZ, err = time.LoadLocation("America/Los_Angeles")
	if err != nil {
		pacificTZ = time.UTC
	}

	tmpl, err := template.New("dashboard").Funcs(template.FuncMap{
		"formatTime": func(t time.Time) string {
			return t.Format("2006-01-02 3:04 PM MST")
		},
		"formatTimeHuman": func(t time.Time) string {
			return "just now"
		},
		"formatPct": func(f float64) string {
			return "0.0%"
		},
		"formatFloat": func(f float64) string {
			return "0.0"
		},
		"formatCoord": func(f float64) string {
			return "0.00000"
		},
		"mapsURL": func(lat, lon float64) string {
			return "https://maps.google.com"
		},
		"gtZero": func(f float64) bool {
			return f > 0
		},
		"formatHours": func(f float64) string {
			return "0"
		},
		"formatMinPerDay": func(f float64) string {
			return "0 min/day"
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
			return date
		},
		"busTypeClass": func(t string) string {
			if t == "BEB" {
				return "beb"
			}
			return "diesel"
		},
		"isAtWalnutCreekBART": func(lat, lon float64) bool {
			return false
		},
		"walnutCreekBARTURL": func() string {
			return "https://maps.google.com"
		},
	}).Parse(htmlTemplate)
	if err != nil {
		t.Fatalf("failed to parse template: %v", err)
	}

	// Test with BEB drought data
	data := DashboardData{
		GeneratedAt:      time.Now(),
		Routes:           "4, 5",
		TotalValidDays:   100,
		DataStartDate:    "2024-01-01",
		DataEndDate:      "2024-04-10",
		ShowBEBDrought:   true,
		LastAnyBEB:       "January 15, 2024",
		LastAnyBEBID:     "1600",
		DaysSinceLastBEB: 45,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("failed to execute template with BEB drought data: %v", err)
	}

	output := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("No BEB has run")) {
		t.Error("template should contain BEB drought message when ShowBEBDrought is true")
	}
	if !bytes.Contains(buf.Bytes(), []byte("45 days")) {
		t.Errorf("template should show days since last BEB, got: %s", output[:500])
	}
}

func TestFindLastAnyBEB(t *testing.T) {
	tripsByDate := map[string][]TripInfo{
		"20240101": {
			{Date: "20240101", VehicleID: "1600", Type: "BEB", Weekday: time.Monday},
		},
		"20240115": {
			{Date: "20240115", VehicleID: "500", Type: "diesel", Weekday: time.Monday},
		},
		"20240110": {
			{Date: "20240110", VehicleID: "1801", Type: "BEB", Weekday: time.Wednesday},
		},
	}

	date, vehicleID := findLastAnyBEB(tripsByDate)
	if date != "20240110" {
		t.Errorf("expected date 20240110, got %s", date)
	}
	if vehicleID != "1801" {
		t.Errorf("expected vehicle 1801, got %s", vehicleID)
	}
}

func TestMergeRealtimeTripsUpdatesBusStats(t *testing.T) {
	// Simulate historic data: bus 1600 last seen on Jan 1
	busStats := map[string]*BusStats{
		"1600": {
			VehicleID:   "1600",
			Type:        "BEB",
			TripCount:   2,
			DayTripMaps: map[string]int{"20240101": 2},
		},
	}
	tripsByDate := map[string][]TripInfo{
		"20240101": {
			{Date: "20240101", VehicleID: "1600", Type: "BEB", Weekday: time.Monday},
			{Date: "20240101", VehicleID: "1600", Type: "BEB", Weekday: time.Monday},
		},
	}
	validDates := []string{"20240101"}

	// Simulate realtime data: bus 1600 seen today, and bus 1801 (new) seen today
	realtimeTrips := map[string][]TripInfo{
		"20240401": {
			{Date: "20240401", VehicleID: "1600", Type: "BEB", Weekday: time.Monday},
			{Date: "20240401", VehicleID: "1801", Type: "BEB", Weekday: time.Monday},
		},
	}

	validDates = mergeRealtimeTrips(realtimeTrips, tripsByDate, busStats, validDates)

	// Bus 1600's DayTripMaps should now include the realtime date
	if _, ok := busStats["1600"].DayTripMaps["20240401"]; !ok {
		t.Error("bus 1600 DayTripMaps missing realtime date 20240401")
	}
	if busStats["1600"].TripCount != 3 {
		t.Errorf("bus 1600 TripCount: got %d, want 3", busStats["1600"].TripCount)
	}

	// Bus 1801 should have been created in busStats from realtime data
	if _, ok := busStats["1801"]; !ok {
		t.Fatal("bus 1801 not added to busStats from realtime data")
	}
	if busStats["1801"].DayTripMaps["20240401"] != 1 {
		t.Errorf("bus 1801 DayTripMaps[20240401]: got %d, want 1", busStats["1801"].DayTripMaps["20240401"])
	}

	// validDates should include the new date
	found := false
	for _, d := range validDates {
		if d == "20240401" {
			found = true
			break
		}
	}
	if !found {
		t.Error("validDates missing 20240401")
	}

	// Verify that computeBusResult picks up the new last service date
	result := computeBusResult(busStats["1600"], validDates, len(validDates))
	if result.LastServiceDate != "20240401" {
		t.Errorf("LastServiceDate: got %s, want 20240401", result.LastServiceDate)
	}
}

func TestFindLastAnyBEBNoBEBs(t *testing.T) {
	tripsByDate := map[string][]TripInfo{
		"20240101": {
			{Date: "20240101", VehicleID: "500", Type: "diesel", Weekday: time.Monday},
		},
	}

	date, vehicleID := findLastAnyBEB(tripsByDate)
	if date != "" {
		t.Errorf("expected empty date, got %s", date)
	}
	if vehicleID != "" {
		t.Errorf("expected empty vehicle, got %s", vehicleID)
	}
}
