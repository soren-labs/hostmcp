package host

import (
	"strings"
	"unicode/utf8"
)

// cleanTerminalText turns raw terminal output into plain text for a model to
// read. It removes ANSI escape sequences (CSI, OSC, DCS/SOS/PM/APC strings and
// two-byte escapes) and other C0 controls except newline and tab, applies
// backspaces, and treats a carriage return not followed by a newline as
// "rewrite this line": text written after it replaces the line, so a progress
// bar redrawn a thousand times leaves only its final state. It is not a
// terminal emulator: cursor movement is dropped, not replayed. A sequence cut
// off at the end of the text is dropped.
func cleanTerminalText(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	line := make([]byte, 0, 256)
	rewrite := false // a bare CR was seen; the next printable text replaces the line
	flush := func() {
		out.Write(line)
		line = line[:0]
	}
	put := func(text string) {
		if rewrite {
			line = line[:0]
			rewrite = false
		}
		line = append(line, text...)
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i = skipEscape(s, i)
			continue
		case c == '\n':
			rewrite = false
			flush()
			out.WriteByte('\n')
		case c == '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
				rewrite = false
				flush()
				out.WriteByte('\n')
			} else {
				rewrite = true
			}
		case c == '\b':
			if !rewrite && len(line) > 0 {
				_, size := utf8.DecodeLastRune(line)
				line = line[:len(line)-size]
			}
		case c == '\t':
			put("\t")
		case c < 0x20 || c == 0x7f:
			// Other C0 controls (BEL, SO/SI, ...) carry no text.
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r >= 0x80 && r <= 0x9f {
				// C1 controls; U+009B is a single-character CSI.
				if r == 0x9b {
					i = skipCSI(s, i+size)
					continue
				}
				i += size
				continue
			}
			put(s[i : i+size])
			i += size
			continue
		}
		i++
	}
	flush()
	return out.String()
}

// skipEscape returns the index after the escape sequence starting at s[i].
func skipEscape(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[':
		return skipCSI(s, i+2)
	case ']', 'P', 'X', '^', '_':
		// OSC ends with BEL or ST (ESC \); the others end with ST.
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 && s[i+1] == ']' {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	}
	// nF escapes: intermediate bytes 0x20-0x2F, then one final byte.
	j := i + 1
	for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
		j++
	}
	if j < len(s) {
		j++
	}
	return j
}

// skipCSI skips parameter (0x30-0x3F) and intermediate (0x20-0x2F) bytes and
// the final byte (0x40-0x7E) of a control sequence whose body starts at s[i].
func skipCSI(s string, i int) int {
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		i++
	}
	return i
}
