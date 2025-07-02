package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/samber/lo"
)

// NodeEvent represents a single node lifecycle event
type NodeEvent struct {
	EventType string
	NodeName  string
	Timestamp time.Time
}

// NodeTiming holds calculated timing data for a node
type NodeTiming struct {
	NodeName               string
	UnhealthyToStartDelete *int64
	UnhealthyToEndDelete   *int64
	EventTimestamps        map[string]time.Time
}

// parseTimestamp handles different timestamp formats
func parseTimestamp(timestampStr string) (time.Time, error) {
	// Handle UTC format like 2025-07-01T19:51:34Z
	if strings.HasSuffix(timestampStr, "Z") {
		return time.Parse(time.RFC3339, timestampStr)
	}
	// Handle timezone format like 2025-07-01T12:52:06-07:00
	return time.Parse(time.RFC3339, timestampStr)
}

// processEvents parses the input data and calculates timing differences
func processEvents(reader io.Reader) ([]NodeTiming, error) {
	csvReader := csv.NewReader(reader)

	// Read all records
	records, err := csvReader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("error reading CSV: %w", err)
	}

	// Skip header if present
	startIdx := 0
	if len(records) > 0 && strings.Contains(records[0][0], "Event") {
		startIdx = 1
	}

	// Group events by node
	nodeEvents := make(map[string]map[string]time.Time)

	for i := startIdx; i < len(records); i++ {
		record := records[i]
		if len(record) != 3 {
			continue
		}

		eventType := strings.TrimSpace(record[0])
		nodeName := strings.TrimSpace(record[1])
		timestampStr := strings.TrimSpace(record[2])

		timestamp, err := parseTimestamp(timestampStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing timestamp '%s': %v\n", timestampStr, err)
			continue
		}

		if nodeEvents[nodeName] == nil {
			nodeEvents[nodeName] = make(map[string]time.Time)
		}
		nodeEvents[nodeName][eventType] = timestamp
	}

	// Calculate timing differences
	var results []NodeTiming

	for nodeName, events := range nodeEvents {
		timing := NodeTiming{
			NodeName:        nodeName,
			EventTimestamps: events,
		}

		// Calculate NodeUnhealthy to StartNodeDelete time
		if unhealthyTime, hasUnhealthy := events["NodeUnhealthy"]; hasUnhealthy {
			if startNodeDeleteTime, hasStartNodeDelete := events["StartNodeDelete"]; hasStartNodeDelete {
				diff := startNodeDeleteTime.Sub(unhealthyTime).Milliseconds() - 30000 // We have a 30s toleration duration
				timing.UnhealthyToStartDelete = &diff
			}
		}

		// Calculate AWSNodeKubeProxyScheduled to NodeReady time
		if unhealthyTime, hasUnhealthy := events["NodeUnhealthy"]; hasUnhealthy {
			if endNodeDeleteTime, hasEndNodeDelete := events["StartNodeDelete"]; hasEndNodeDelete {
				diff := endNodeDeleteTime.Sub(unhealthyTime).Milliseconds() - 30000 // We have a 30s toleration duration
				timing.UnhealthyToEndDelete = &diff
			}
		}
		results = append(results, timing)
	}

	// Sort results by node name for consistent output
	sort.Slice(results, func(i, j int) bool {
		return results[i].NodeName < results[j].NodeName
	})

	return results, nil
}

// writeCSV outputs the results in CSV format
func writeCSV(results []NodeTiming, writer io.Writer) error {
	csvWriter := csv.NewWriter(writer)
	defer csvWriter.Flush()

	// Collect all unique event types for timestamp columns
	eventTypes := make(map[string]bool)
	for _, result := range results {
		for eventType := range result.EventTimestamps {
			eventTypes[eventType] = true
		}
	}

	// Sort event types for consistent column order
	var sortedEventTypes []string
	for eventType := range eventTypes {
		sortedEventTypes = append(sortedEventTypes, eventType)
	}
	sort.Strings(sortedEventTypes)

	// Build headers
	headers := []string{
		"node",
		"unhealthy_to_start_delete",
		"unhealthy_to_end_delete",
	}

	// Add timestamp headers
	for _, eventType := range sortedEventTypes {
		headers = append(headers, eventType)
	}

	// Write header
	if err := csvWriter.Write(headers); err != nil {
		return fmt.Errorf("error writing header: %w", err)
	}

	// Write data rows
	for _, result := range results {
		row := make([]string, len(headers))

		row[0] = result.NodeName

		if result.UnhealthyToStartDelete != nil {
			row[1] = fmt.Sprintf("%d", *result.UnhealthyToStartDelete)
		}

		if result.UnhealthyToEndDelete != nil {
			row[2] = fmt.Sprintf("%d", *result.UnhealthyToEndDelete)
		}

		// Add timestamp values
		for i, eventType := range sortedEventTypes {
			if timestamp, exists := result.EventTimestamps[eventType]; exists {
				row[4+i] = timestamp.Format(time.RFC3339)
			}
		}
		if err := csvWriter.Write(row); err != nil {
			return fmt.Errorf("error writing row: %w", err)
		}
	}

	return nil
}

func main() {
	// Command line flags
	inputFile := flag.String("i", "", "Input file (default: stdin)")
	outputFile := flag.String("o", "", "Output CSV file (default: stdout)")
	flag.Parse()

	if lo.FromPtr(inputFile) == "" || lo.FromPtr(outputFile) == "" {
		log.Fatal("input file AND output file must be provided")
		os.Exit(1)
	}

	// Open input
	var reader io.Reader
	if *inputFile != "" {
		file, err := os.Open(*inputFile)
		if err != nil {
			log.Fatalf("Error opening input file: %v", err)
		}
		defer file.Close()
		reader = file
	} else {
		reader = os.Stdin
	}

	// Process events
	results, err := processEvents(reader)
	if err != nil {
		log.Fatalf("Error processing events: %v", err)
	}

	// Open output
	var writer io.Writer
	if *outputFile != "" {
		file, err := os.Create(*outputFile)
		if err != nil {
			log.Fatalf("Error creating output file: %v", err)
		}
		defer file.Close()
		writer = file
		fmt.Fprintf(os.Stderr, "CSV output written to %s\n", *outputFile)
	} else {
		writer = os.Stdout
	}

	// Write CSV output
	if err := writeCSV(results, writer); err != nil {
		log.Fatalf("Error writing CSV: %v", err)
	}
}
