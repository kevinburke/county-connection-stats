# County Connection Vehicle Tracker

A simple Go script to fetch real-time vehicle positions for County Connection (CCCTA) buses using the 511.org GTFS Realtime API.

## Prerequisites

- Go 1.16 or later
- A free API key from 511.org

## Setup

### 1. Get an API Key

Request a free API key from 511.org:
https://511.org/open-data/token

You'll receive the key via email after verifying your email address.

### 2. Install Dependencies

```bash
# Initialize Go module
go mod init county-connection-tracker

# Download dependencies
go get google.golang.org/protobuf/proto
go get github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs
```

### 3. Set Your API Key

```bash
# Linux/Mac
export API_511_KEY="your-api-key-here"

# Windows (Command Prompt)
set API_511_KEY=your-api-key-here

# Windows (PowerShell)
$env:API_511_KEY="your-api-key-here"
```

## Usage

### Run the script:

```bash
go run county_connection_tracker.go
```

### Build and run:

```bash
go build county_connection_tracker.go
./county_connection_tracker
```

## Output

The script displays:
- Vehicle ID (the specific bus number)
- Route ID
- Trip ID
- Current GPS position (latitude/longitude)
- Speed (if available)
- Bearing/heading (if available)
- Last update timestamp

## Example Output

```
County Connection Vehicle Positions (2025-01-15 14:30:45)
================================================================================

Vehicle ID:  1234
Route:       15
Trip ID:     CC_2024_WD_15_0830
Position:    37.943920, -122.056789
Speed:       8.5 m/s (19.0 mph)
Bearing:     135.0°
Updated:     14:30:32
--------------------------------------------------------------------------------

Vehicle ID:  5678
Route:       7
Trip ID:     CC_2024_WD_7_1445
Position:    37.925341, -122.031456
Speed:       0.0 m/s (0.0 mph)
Updated:     14:30:40
--------------------------------------------------------------------------------

Total active vehicles: 24
```

## Collecting Data for Reliability Analysis

### Vehicle Tracker Tool

The `cmd/tracker` directory contains a production-ready vehicle tracking tool that continuously monitors buses and logs positions to a TSV file.

**Features:**
- Monitors multiple routes simultaneously (e.g., routes 4 and 5)
- Decoupled API polling from data processing
- TSV output for easy analysis
- Configurable polling and processing intervals

**Local Usage:**
```bash
# Track routes 4 and 5, polling every 2 minutes
export API_511_KEY="your-api-key-here"
go run ./cmd/tracker -route 4,5 -api-poll-interval 2m -process-interval 2m
```

**Available Options:**
- `-route` - Route number(s) to track, comma-separated (default: 4)
- `-api-poll-interval` - How often to fetch from API (default: 1m, max 60/hour)
- `-process-interval` - How often to process/write data (default: 1m)
- `-output` - TSV file for vehicle observations (default: vehicle-tracking.tsv)

### Production Deployment

The tracker is deployed to production servers as a systemd service using Ansible. The deployment:
- Creates a dedicated `county-connection` user
- Installs the tracker binary to `/home/county-connection/bin/`
- Sets up a systemd service that runs continuously
- Polls the API every 2 minutes and writes data to `/home/county-connection/var/data/vehicle-tracking.tsv`
- Automatically restarts the service if it crashes
- Rotates data files monthly (keeps 12 months of history, with compression)

**To deploy:**

1. Build the Linux binary:
   ```bash
   make build
   ```

2. Deploy to tiger server:
   ```bash
   cd ../web-deployment
   make county-connection group=tiger_group
   ```

The deployment configuration is in `../web-deployment/county-connection.yml`.

**Managing the service on the server:**

```bash
# Check service status
sudo systemctl status county-connection-tracker

# View logs
sudo journalctl -u county-connection-tracker -f

# Restart service
sudo systemctl restart county-connection-tracker

# Stop service
sudo systemctl stop county-connection-tracker
```

## Dashboard

The `cmd/dashboard` tool generates a static HTML dashboard showing BEB vs diesel reliability statistics for routes 4 and 5. The dashboard is served at https://kevin.burke.dev/data/county-connection-bus-tracker/

### What the Dashboard Shows

- **Live Status**: Current buses running on routes 4 & 5
- **Weekend BEB Service**: Last time a battery electric bus ran on a weekend (spoiler: June 18, 2022)
- **BEB Fleet Reliability**: Stats for all 8 BEBs (1600-1603, 1800-1803) including availability, MDBF, outage periods
- **Time Period Analysis**: BEB vs diesel split for last week, month, and year
- **Wasted Time**: Hours wasted by diesel buses waiting at BART during charging cycles they don't need

### Local Development

```bash
# Sync tracking data from server and generate dashboard locally
make dashboard-sync

# Or just generate dashboard (if you already have vehicle-tracking.tsv)
make dashboard
```

The dashboard is written to `var/dashboard.html`.

### Updating the Dashboard Code

After making changes to `cmd/dashboard/main.go`:

1. **Build the Linux binary:**
   ```bash
   make build
   ```

2. **Deploy to the server:**
   ```bash
   cd ../web-deployment
   make county-connection group=tiger_group
   ```

   This will:
   - Copy the new dashboard binary to `/home/county-connection/bin/dashboard`
   - Restart the systemd timer that regenerates the dashboard every 5 minutes

3. **Verify the deployment:**
   ```bash
   # Check the timer is running
   ssh tiger-root "systemctl status county-connection-dashboard.timer"

   # Check the service ran successfully
   ssh tiger-root "systemctl status county-connection-dashboard.service"

   # View recent logs
   ssh tiger-root "journalctl -u county-connection-dashboard.service -n 20"
   ```

### Updating Historic Data

If you have new extracted GTFS data (stop observations):

1. Place the file at `var/extracted/cc45-stop-observations.txt`

2. Deploy:
   ```bash
   make build
   cd ../web-deployment
   make county-connection group=tiger_group
   ```

   The Ansible playbook copies `historic_data_local_path` to the server.

### Dashboard Configuration

The dashboard runs as a systemd timer on the server:

- **Service**: `county-connection-dashboard.service` (oneshot, runs the dashboard binary)
- **Timer**: `county-connection-dashboard.timer` (triggers every 5 minutes)
- **Output**: `/home/kevin-burke-dev/public/data/county-connection-bus-tracker/index.html`
- **Data sources**:
  - Real-time: `/home/county-connection/var/data/vehicle-tracking.tsv`
  - Historic: `/home/county-connection/var/extracted/cc45-stop-observations.txt`

### Managing the Dashboard on the Server

```bash
# Check timer status
sudo systemctl status county-connection-dashboard.timer

# Manually trigger a dashboard regeneration
sudo systemctl start county-connection-dashboard.service

# View logs
sudo journalctl -u county-connection-dashboard.service -f

# Restart the timer
sudo systemctl restart county-connection-dashboard.timer
```

## API Rate Limits

- Default: 60 requests per hour per API key
- For higher limits, contact: 511sfbaydeveloperresources@googlegroups.com

## Data Notes

- The 511 API updates vehicle positions approximately every 30 seconds
- Not all vehicles may report all data fields (speed, bearing, etc.)
- Vehicle IDs should match the physical bus numbers on the vehicles

## Resources

- [511.org Open Data Portal](https://511.org/open-data)
- [GTFS Realtime Reference](https://gtfs.org/realtime/)
- [511 Developer Resources Google Group](https://groups.google.com/g/511sfbaydeveloperresources)
