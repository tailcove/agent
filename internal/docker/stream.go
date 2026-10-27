package docker

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// maxFrame bounds one frame of the engine's multiplexed stream; a larger one means the stream is not what it should be.
const maxFrame = 1 << 20

// maxLine bounds a line that has no newline yet; it is cut there and the rest is a new line.
const maxLine = 64 << 10

// Chunk is a piece of one stream's output.
type Chunk struct {
	Stream string
	Data   []byte
}

// Demux reads the engine's log stream. Without a terminal every frame has an
// 8 byte header (the stream: 1 stdout, 2 stderr; three zero bytes; the size, big
// endian); with a terminal the engine sends the bytes as they are, all of them
// stdout. It calls emit for each piece until the stream ends.
func Demux(reader io.Reader, tty bool, emit func(Chunk)) error {
	buffered := bufio.NewReaderSize(reader, 64<<10)

	if tty {
		buf := make([]byte, 32<<10)

		for {
			n, err := buffered.Read(buf)
			if n > 0 {
				emit(Chunk{Stream: "stdout", Data: append([]byte(nil), buf[:n]...)})
			}

			if err != nil {
				return ignoreEnd(err)
			}
		}
	}

	var header [8]byte

	for {
		if _, err := io.ReadFull(buffered, header[:]); err != nil {
			return ignoreEnd(err)
		}

		size := binary.BigEndian.Uint32(header[4:])
		if size > maxFrame || header[1] != 0 || header[2] != 0 || header[3] != 0 || header[0] > 2 {
			return errors.New("docker: the log stream is not in the expected format")
		}

		data := make([]byte, size)
		if _, err := io.ReadFull(buffered, data); err != nil {
			return ignoreEnd(err)
		}

		stream := "stdout"
		if header[0] == 2 {
			stream = "stderr"
		}

		emit(Chunk{Stream: stream, Data: data})
	}
}

func ignoreEnd(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}

	return err
}

// Line is one line of output with the time the engine stamped on it.
type Line struct {
	Stream string
	Time   time.Time
	Text   string
}

// Lines turns chunks into lines. A frame can end in the middle of a line, so
// the text after the last newline waits, per stream, for the rest.
type Lines struct {
	pending map[string][]byte
	// continued holds, per stream, the time of a line that was cut for being
	// too long: the rest of it has no time of its own.
	continued map[string]time.Time
}

// NewLines returns an empty splitter.
func NewLines() *Lines {
	return &Lines{pending: map[string][]byte{}, continued: map[string]time.Time{}}
}

// Add returns the lines the chunk completed.
func (l *Lines) Add(chunk Chunk) []Line {
	data := append(l.pending[chunk.Stream], chunk.Data...)

	var lines []Line

	for {
		newline := indexByte(data, '\n')
		if newline < 0 {
			break
		}

		if line, ok := l.parse(chunk.Stream, data[:newline]); ok {
			lines = append(lines, line)
		}

		data = data[newline+1:]
	}

	// A line that never ends is cut, so a stream without newlines cannot fill the memory.
	for len(data) > maxLine {
		if line, ok := l.parse(chunk.Stream, data[:maxLine]); ok {
			lines = append(lines, line)
			l.continued[chunk.Stream] = line.Time
		}

		data = data[maxLine:]
	}

	l.pending[chunk.Stream] = append([]byte(nil), data...)

	return lines
}

// parse reads a line, or the rest of one that was cut.
func (l *Lines) parse(stream string, raw []byte) (Line, bool) {
	if at, cut := l.continued[stream]; cut {
		delete(l.continued, stream)

		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}

		return Line{Stream: stream, Time: at, Text: string(raw)}, len(raw) > 0
	}

	return parseLine(stream, raw)
}

func indexByte(data []byte, b byte) int {
	for i, c := range data {
		if c == b {
			return i
		}
	}

	return -1
}

// parseLine splits "2026-10-27T10:00:00.123456789Z text" and drops the carriage return of a terminal.
func parseLine(stream string, raw []byte) (Line, bool) {
	if len(raw) > 0 && raw[len(raw)-1] == '\r' {
		raw = raw[:len(raw)-1]
	}

	space := indexByte(raw, ' ')
	if space < 20 {
		return Line{}, false
	}

	at, err := time.Parse(time.RFC3339Nano, string(raw[:space]))
	if err != nil {
		return Line{}, false
	}

	return Line{Stream: stream, Time: at.UTC(), Text: string(raw[space+1:])}, true
}
