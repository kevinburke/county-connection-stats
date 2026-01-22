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

.PHONY: sync-tracking
sync-tracking:
	scp $(TRACKER_HOST):$(TRACKER_PATH) vehicle-tracking.tsv

.PHONY: dashboard-run
dashboard-run:
	go run -trimpath ./cmd/dashboard

.PHONY: dashboard-sync
dashboard-sync: sync-tracking dashboard-run

.PHONY: serve
serve: dashboard-run
	@echo "Serving dashboard at http://localhost:8080/var/dashboard.html"
	python3 -m http.server 8080

.PHONY: test
test:
	go test -trimpath ./...
