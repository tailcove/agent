package tail

import (
	"encoding/json"
	"strings"
	"time"
)

// Line formats a source can be written in.
const (
	FormatRaw    = "raw"
	FormatCRI    = "cri"
	FormatDocker = "docker"
	FormatAuto   = "auto"
)

// decoded is one physical line with the wrapping a container runtime put around
// it taken off.
type decoded struct {
	text    string
	time    time.Time
	stream  string
	partial bool
}

// decode takes a runtime's wrapper off a line. The CRI format (containerd,
// CRI-O) is "<RFC 3339 time> <stdout|stderr> <F|P> <text>", where P marks a
// piece of a longer line. The Docker json-file format is a JSON object with
// log, stream and time. A line that does not look like its format is passed on
// as it is: the agent never drops a line because it could not read it.
func decode(format, line string, now time.Time) decoded {
	switch format {
	case FormatCRI:
		if d, ok := decodeCRI(line); ok {
			return d
		}
	case FormatDocker:
		if d, ok := decodeDocker(line); ok {
			return d
		}
	case FormatAuto:
		if strings.HasPrefix(line, "{") {
			if d, ok := decodeDocker(line); ok {
				return d
			}
		} else if d, ok := decodeCRI(line); ok {
			return d
		}
	}

	return decoded{text: line, time: now}
}

func decodeCRI(line string) (decoded, bool) {
	stamp, rest, found := strings.Cut(line, " ")
	if !found {
		return decoded{}, false
	}

	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return decoded{}, false
	}

	stream, rest, found := strings.Cut(rest, " ")
	if !found || (stream != "stdout" && stream != "stderr") {
		return decoded{}, false
	}

	// The flag is followed by a space and the text; an empty line is the flag alone.
	flag, text, _ := strings.Cut(rest, " ")
	if flag != "F" && flag != "P" {
		return decoded{}, false
	}

	return decoded{text: text, time: at, stream: stream, partial: flag == "P"}, true
}

func decodeDocker(line string) (decoded, bool) {
	var entry struct {
		Log    *string `json:"log"`
		Stream string  `json:"stream"`
		Time   string  `json:"time"`
	}

	if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Log == nil {
		return decoded{}, false
	}

	at, _ := time.Parse(time.RFC3339Nano, entry.Time)

	// A Docker entry holds a line with its newline, or a piece without one.
	text := *entry.Log
	partial := !strings.HasSuffix(text, "\n")

	return decoded{text: strings.TrimRight(text, "\r\n"), time: at, stream: entry.Stream, partial: partial}, true
}
