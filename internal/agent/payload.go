package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/tailcove/agent/internal/hostmetrics"
	"github.com/tailcove/agent/internal/tail"
)

// logEntry turns one line into the JSON object the log endpoint takes. A line
// that is already a JSON object is passed on as it is (with the host added when
// it names none); any other line becomes a message.
func logEntry(line tail.Line, host string) json.RawMessage {
	text := strings.TrimSpace(line.Text)

	if strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}") {
		var object map[string]any

		if err := json.Unmarshal([]byte(text), &object); err == nil {
			if host != "" {
				if _, named := object["host"]; !named {
					if _, named := object["hostname"]; !named {
						object["host"] = host
					}
				}
			}

			if encoded, err := json.Marshal(object); err == nil {
				return encoded
			}
		}
	}

	encoded, _ := json.Marshal(map[string]any{
		"message": line.Text,
		"ts":      line.Time.UTC().Format(time.RFC3339Nano),
		"host":    host,
		"context": map[string]string{"file": line.File},
	})

	return encoded
}

// logsPayload is a JSON array of entries.
func logsPayload(entries []json.RawMessage) []byte {
	return joinArray(entries)
}

// metricPoint is the native metrics shape.
type metricPoint struct {
	Name   string            `json:"name"`
	Value  float64           `json:"value"`
	Host   string            `json:"host,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	TS     string            `json:"ts"`
}

// metricsPayloads turns points into request bodies of at most perRequest points.
func metricsPayloads(points []hostmetrics.Point, host string, at time.Time, perRequest int) [][]byte {
	stamp := at.UTC().Format(time.RFC3339Nano)

	var payloads [][]byte

	for start := 0; start < len(points); start += perRequest {
		end := min(start+perRequest, len(points))
		entries := make([]json.RawMessage, 0, end-start)

		for _, point := range points[start:end] {
			encoded, err := json.Marshal(metricPoint{Name: point.Name, Value: point.Value, Host: host, Labels: point.Labels, TS: stamp})
			if err != nil {
				continue
			}

			entries = append(entries, encoded)
		}

		payloads = append(payloads, joinArray(entries))
	}

	return payloads
}

func joinArray(entries []json.RawMessage) []byte {
	var buffer bytes.Buffer

	buffer.WriteByte('[')

	for i, entry := range entries {
		if i > 0 {
			buffer.WriteByte(',')
		}

		buffer.Write(entry)
	}

	buffer.WriteByte(']')

	return buffer.Bytes()
}
