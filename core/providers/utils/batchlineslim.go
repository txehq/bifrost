package utils

import (
	"encoding/json"
)

// A batch result line larger than client.max_response_body_size_mb on its own fits no page
// (TXE-7526). Its bulk is almost always one value, a base64 image, a long completion or an
// embedding vector, while what settlement bills (its custom_id, model, status and usage) is
// a handful of short values. lineSlimmer streams such a line through without holding it and
// rewrites it as a small JSON line of the same shape: every key is kept, a string longer than
// slimStringBytes becomes "", and an array keeps its first slimArrayItems elements. The
// provider then reads the slim line with its own line parser, so every vendor's shape
// (OpenAI's response.body.usage, Gemini's usageMetadata, Anthropic's result.message.usage)
// keeps its usage without this file knowing any of them.
const (
	slimStringBytes = 1024
	slimArrayItems  = 16
	slimLineBytes   = 64 * 1024
	slimDepth       = 64
	slimScalarBytes = 64
)

type slimFrame struct {
	object bool
	keep   bool // whether this container's own bytes are written
	phase  slimPhase
	count  int  // elements (array) or members (object) begun
	wrote  int  // of those, how many were written
	member bool // object: whether the current member is written
}

type slimPhase int

const (
	slimExpectValue slimPhase = iota // array: a value or ]; object (after the colon): a value
	slimExpectKey                    // object: a key or }
	slimExpectColon
	slimExpectNext // after a value: a comma or the close
)

type lineSlimmer struct {
	out    []byte
	stack  []slimFrame
	failed bool
	done   bool // the top-level value is complete

	inString  bool
	escape    bool
	stringRaw []byte
	stringLen int
	stringOut bool
	stringKey bool

	inScalar  bool
	scalarRaw []byte
	scalarOut bool
}

func newLineSlimmer() *lineSlimmer {
	return &lineSlimmer{out: make([]byte, 0, 4096)}
}

func (s *lineSlimmer) fail() { s.failed = true; s.out = nil; s.stringRaw = nil; s.scalarRaw = nil }

func (s *lineSlimmer) emit(b ...byte) {
	if s.failed {
		return
	}
	if len(s.out)+len(b) > slimLineBytes {
		s.fail()
		return
	}
	s.out = append(s.out, b...)
}

// Write feeds the next bytes of the line.
func (s *lineSlimmer) Write(p []byte) {
	for _, b := range p {
		if s.failed {
			return
		}
		s.feed(b)
	}
}

// Result returns the slim line, or nil when the line was not one JSON value the slimmer could
// keep within slimLineBytes.
func (s *lineSlimmer) Result() []byte {
	if s.inScalar {
		s.endScalar()
	}
	if s.failed || !s.done || len(s.stack) != 0 || s.inString || !json.Valid(s.out) {
		return nil
	}
	return s.out
}

// beginValue decides whether the value starting now is written, writing the comma before it.
func (s *lineSlimmer) beginValue() bool {
	if len(s.stack) == 0 {
		if s.done {
			s.fail()
			return false
		}
		return true
	}
	top := &s.stack[len(s.stack)-1]
	if top.phase != slimExpectValue {
		s.fail()
		return false
	}
	top.phase = slimExpectNext
	if top.object {
		return top.member
	}
	index := top.count
	top.count++
	keep := top.keep && index < slimArrayItems
	if keep {
		if top.wrote > 0 {
			s.emit(',')
		}
		top.wrote++
	}
	return keep
}

// endValue closes a value; at the top level it is the whole line.
func (s *lineSlimmer) endValue() {
	if len(s.stack) == 0 {
		s.done = true
	}
}

func (s *lineSlimmer) feed(b byte) {
	if s.inString {
		s.feedString(b)
		return
	}
	if s.inScalar {
		if isScalarByte(b) {
			if len(s.scalarRaw) >= slimScalarBytes {
				s.fail()
				return
			}
			s.scalarRaw = append(s.scalarRaw, b)
			return
		}
		s.endScalar()
		if s.failed {
			return
		}
	}
	switch b {
	case ' ', '\t', '\r', '\n':
		return
	case '"':
		s.beginString()
	case '{', '[':
		keep := s.beginValue()
		if s.failed {
			return
		}
		if len(s.stack) >= slimDepth {
			s.fail()
			return
		}
		frame := slimFrame{object: b == '{', keep: keep, phase: slimExpectValue}
		if frame.object {
			frame.phase = slimExpectKey
		}
		if keep {
			s.emit(b)
		}
		s.stack = append(s.stack, frame)
	case '}', ']':
		if len(s.stack) == 0 {
			s.fail()
			return
		}
		top := s.stack[len(s.stack)-1]
		if top.object != (b == '}') {
			s.fail()
			return
		}
		// Empty containers close from their first phase; others close after a value.
		if top.phase != slimExpectNext && !(top.count == 0 && (top.phase == slimExpectKey || top.phase == slimExpectValue)) {
			s.fail()
			return
		}
		s.stack = s.stack[:len(s.stack)-1]
		if top.keep {
			s.emit(b)
		}
		s.endValue()
	case ',':
		if len(s.stack) == 0 || s.stack[len(s.stack)-1].phase != slimExpectNext {
			s.fail()
			return
		}
		top := &s.stack[len(s.stack)-1]
		if top.object {
			top.phase = slimExpectKey
		} else {
			top.phase = slimExpectValue
		}
	case ':':
		if len(s.stack) == 0 || s.stack[len(s.stack)-1].phase != slimExpectColon {
			s.fail()
			return
		}
		s.stack[len(s.stack)-1].phase = slimExpectValue
		if s.stack[len(s.stack)-1].member {
			s.emit(':')
		}
	default:
		if !isScalarByte(b) {
			s.fail()
			return
		}
		s.scalarOut = s.beginValue()
		if s.failed {
			return
		}
		s.inScalar = true
		s.scalarRaw = append(s.scalarRaw[:0], b)
	}
}

func (s *lineSlimmer) beginString() {
	if len(s.stack) > 0 && s.stack[len(s.stack)-1].object && s.stack[len(s.stack)-1].phase == slimExpectKey {
		top := &s.stack[len(s.stack)-1]
		top.count++
		top.member = top.keep
		if top.member {
			if top.wrote > 0 {
				s.emit(',')
			}
			top.wrote++
		}
		top.phase = slimExpectColon
		s.stringKey = true
		s.stringOut = top.member
	} else {
		s.stringKey = false
		s.stringOut = s.beginValue()
		if s.failed {
			return
		}
	}
	s.inString = true
	s.escape = false
	s.stringLen = 0
	s.stringRaw = s.stringRaw[:0]
}

func (s *lineSlimmer) feedString(b byte) {
	if s.escape {
		s.escape = false
	} else if b == '\\' {
		s.escape = true
	} else if b == '"' {
		s.inString = false
		if s.stringOut {
			if s.stringLen > slimStringBytes {
				s.emit('"', '"')
			} else {
				s.emit('"')
				s.emit(s.stringRaw...)
				s.emit('"')
			}
		}
		if !s.stringKey {
			s.endValue()
		}
		return
	} else if b < 0x20 {
		s.fail()
		return
	}
	s.stringLen++
	if s.stringOut && s.stringLen <= slimStringBytes {
		s.stringRaw = append(s.stringRaw, b)
	}
}

func (s *lineSlimmer) endScalar() {
	s.inScalar = false
	if !json.Valid(s.scalarRaw) {
		s.fail()
		return
	}
	if s.scalarOut {
		s.emit(s.scalarRaw...)
	}
	s.endValue()
}

func isScalarByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || b == '-' || b == '+' || b == '.' || b == 'E'
}
