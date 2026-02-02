# County Connection Reliability Tracker Makefile

.PHONY: build
build:
	mkdir -p bin
	GOOS=linux GOARCH=amd64 go build -trimpath -o bin/tracker ./cmd/tracker
	GOOS=linux GOARCH=amd64 go build -trimpath -o bin/dashboard ./cmd/dashboard

.PHONY: build-local
build-local:
	mkdir -p bin
	go build -trimpath -o bin/tracker ./cmd/tracker

.PHONY: tracker
tracker:
	mkdir -p bin/amd64
	GOOS=linux GOARCH=amd64 go build -trimpath -o bin/amd64/tracker ./cmd/tracker

.PHONY: dashboard
dashboard:
	mkdir -p bin/amd64
	GOOS=linux GOARCH=amd64 go build -trimpath -o bin/amd64/dashboard ./cmd/dashboard

.PHONY: linux-amd64
linux-amd64: tracker dashboard

.PHONY: run
run:
	go run ./cmd/tracker

.PHONY: fmt
fmt:
	go fmt ./...
	goimports -w .

.PHONY: clean
clean:
	rm -rf bin/

# Server to download tracking data from
TRACKER_HOST ?= tiger-root
TRACKER_PATH ?= /home/county-connection/var/data/vehicle-tracking.tsv

# Backup server for historic data
BACKUP_HOST ?= ts-ostrich
BACKUP_PATH ?= /volumeUSB1/usbshare/data-files/county-connection-reliability

.PHONY: sync-tracking
sync-tracking:
	scp $(TRACKER_HOST):$(TRACKER_PATH) vehicle-tracking.tsv

.PHONY: backup-data
backup-data:
	rsync -av --rsync-path=/usr/bin/rsync -e "ssh -o ServerAliveInterval=60 -o ServerAliveCountMax=3" --progress ./var/ $(BACKUP_HOST):$(BACKUP_PATH)/

.PHONY: restore-data
restore-data:
	rsync -av --rsync-path=/usr/bin/rsync -e "ssh -o ServerAliveInterval=60 -o ServerAliveCountMax=3" --progress $(BACKUP_HOST):$(BACKUP_PATH)/ ./var/

.PHONY: dashboard-run
dashboard-run:
	go run -trimpath ./cmd/dashboard

.PHONY: dashboard-sync
dashboard-sync: sync-tracking dashboard-run

.PHONY: serve
serve: dashboard-sync
	@echo "Serving dashboard at http://localhost:8080/var/dashboard.html"
	python3 -m http.server 8080

.PHONY: ontime
ontime: sync-tracking
	go run -trimpath ./cmd/analyze-ontime

.PHONY: ontime-4
ontime-4: sync-tracking
	go run -trimpath ./cmd/analyze-ontime -route 4

.PHONY: ontime-5
ontime-5: sync-tracking
	go run -trimpath ./cmd/analyze-ontime -route 5

.PHONY: timetable
timetable:
	go run -trimpath ./cmd/timetable-generator

.PHONY: triptime
triptime: sync-tracking
	go run -trimpath ./cmd/analyze-triptime -route 5

.PHONY: triptime-4
triptime-4: sync-tracking
	go run -trimpath ./cmd/analyze-triptime -route 4

.PHONY: test
test:
	go test -trimpath ./...

.PHONY: export
export: sync-tracking
	go run -trimpath ./cmd/export-data

.PHONY: export-local
export-local:
	go run -trimpath ./cmd/export-data
