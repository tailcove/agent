// Package source holds what every log reader that is not a file follower
// (journald, Docker) hands to the agent.
package source

import "encoding/json"

// Event is one finished log entry, with the place in its stream a restart can
// resume from once the entry has been delivered.
type Event struct {
	// Entry is the JSON object the log endpoint takes.
	Entry json.RawMessage
	// Key names the stream the cursor belongs to ("journald", "docker:<id>").
	Key string
	// Cursor is where the stream stood after this entry; opaque to the agent.
	Cursor string
}
