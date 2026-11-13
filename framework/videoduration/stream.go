package videoduration

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// ReadStream reads the duration of the MP4 r streams, holding only its header (fork, TXE-7334).
//
// The video settler downloads a rendered video to measure it. Held whole, a file past the gateway's
// response body limit could not be measured, so a long render escaped measurement by being large.
// ReadStream walks the file's top-level boxes as they arrive: each mdat's payload is read past and
// dropped, and every other box (moov, every moof, ftyp) is kept, within limits.MaxHeaderBytes and
// limits.MaxBoxes. Read then runs over a sparse image of the file whose mdat payloads are zero, which
// it never reads. maxBytes bounds the bytes read from r, mdat included; a file past it is
// ErrUnmeasurable. A read error of r, and the context's error, are returned as they are.
func ReadStream(ctx context.Context, r io.Reader, limits ReadLimits, maxBytes int64) (Duration, error) {
	src := &streamSource{ctx: ctx, r: r}
	image, err := readTopLevelBoxes(src, limits, maxBytes)
	switch {
	case src.err != nil:
		return Duration{}, src.err
	case err != nil:
		return Duration{}, fmt.Errorf("%w: %w", ErrUnmeasurable, err)
	}
	return ReadWithLimits(ctx, image, image.size, limits)
}

// sparseFile is a file of size bytes of which only segments are held; every other byte reads as 0.
type sparseFile struct {
	segments []sparseSegment
	size     int64
}

type sparseSegment struct {
	offset int64
	data   []byte
}

func (f *sparseFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}
	n := len(p)
	if rest := f.size - off; int64(n) > rest {
		n = int(rest)
	}
	clear(p[:n])
	// Segments are in file order and do not overlap: start at the first that ends past off and stop
	// at the first that starts past the read, so a file of many boxes costs a search per read, not
	// a pass over every box.
	i := sort.Search(len(f.segments), func(i int) bool {
		s := f.segments[i]
		return s.offset+int64(len(s.data)) > off
	})
	for ; i < len(f.segments) && f.segments[i].offset < off+int64(n); i++ {
		s := f.segments[i]
		start, end := max(off, s.offset), min(off+int64(n), s.offset+int64(len(s.data)))
		if start < end {
			copy(p[start-off:end-off], s.data[start-s.offset:end-s.offset])
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// streamSource is r with the caller's context checked before every read, and the first read error
// that is not io.EOF kept: that error is the caller's, never the input's.
type streamSource struct {
	ctx context.Context
	r   io.Reader
	err error
}

func (s *streamSource) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return 0, err
	}
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}

// readTopLevelBoxes reads r to its end, keeping every top-level box but mdat's payload.
func readTopLevelBoxes(r io.Reader, limits ReadLimits, maxBytes int64) (*sparseFile, error) {
	image := &sparseFile{}
	var kept int64
	for boxes := 0; ; boxes++ {
		header := make([]byte, 8, 16)
		n, err := io.ReadFull(r, header)
		if err == io.EOF && n == 0 {
			return image, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: a box header cut short at byte %d", errVideoInvalid, image.size)
		}
		if boxes >= limits.MaxBoxes {
			return nil, fmt.Errorf("%w: more than %d top-level boxes", errVideoReadLimit, limits.MaxBoxes)
		}
		size, typ := int64(binary.BigEndian.Uint32(header[:4])), string(header[4:8])
		toEOF := false
		switch size {
		case 0:
			// The box runs to the end of the file.
			toEOF = true
		case 1:
			large := header[8:16]
			if _, err := io.ReadFull(r, large); err != nil {
				return nil, fmt.Errorf("%w: a box's 64-bit size cut short", errVideoInvalid)
			}
			header = header[:16]
			u := binary.BigEndian.Uint64(large)
			if u > uint64(maxBytes) {
				return nil, fmt.Errorf("%w: a %s box of %d bytes", errVideoReadLimit, typ, u)
			}
			size = int64(u)
		}
		headerLen := int64(len(header))
		if !toEOF && size < headerLen {
			return nil, fmt.Errorf("%w: a %s box of %d bytes", errVideoInvalid, typ, size)
		}
		if !toEOF && image.size > maxBytes-size {
			return nil, fmt.Errorf("%w: the file runs past %d bytes", errVideoReadLimit, maxBytes)
		}
		payload := size - headerLen
		if typ == "mdat" {
			// The media itself: read past it, keep its header so the box structure reads whole.
			image.segments = append(image.segments, sparseSegment{offset: image.size, data: header})
			var skipped int64
			if toEOF {
				skipped, err = io.Copy(io.Discard, io.LimitReader(r, maxBytes-image.size-headerLen+1))
				if err == nil && image.size+headerLen+skipped > maxBytes {
					return nil, fmt.Errorf("%w: the file runs past %d bytes", errVideoReadLimit, maxBytes)
				}
				// The header's size field says "to the end"; the reader needs the size it reached.
				binary.BigEndian.PutUint32(header[:4], uint32(min(headerLen+skipped, 1<<32-1)))
				if headerLen+skipped >= 1<<32 {
					return nil, fmt.Errorf("%w: an mdat to the end of the file past 4 GiB", errVideoReadLimit)
				}
			} else {
				skipped, err = io.CopyN(io.Discard, r, payload)
			}
			if err != nil {
				return nil, fmt.Errorf("%w: the mdat at byte %d cut short", errVideoInvalid, image.size)
			}
			image.size += headerLen + skipped
			if toEOF {
				return image, nil
			}
			continue
		}
		if toEOF {
			return nil, fmt.Errorf("%w: a %s box that runs to the end of the file", errVideoInvalid, typ)
		}
		// A container (moov of a long file) can hold more than MaxBoxBytes; Read holds each box it
		// decodes to that. What is kept is held to MaxHeaderBytes.
		if payload > limits.MaxHeaderBytes-kept {
			return nil, fmt.Errorf("%w: a %s box of %d bytes", errVideoReadLimit, typ, payload)
		}
		kept += payload
		box := make([]byte, size)
		copy(box, header)
		if _, err := io.ReadFull(r, box[headerLen:]); err != nil {
			return nil, fmt.Errorf("%w: the %s box at byte %d cut short", errVideoInvalid, typ, image.size)
		}
		image.segments = append(image.segments, sparseSegment{offset: image.size, data: box})
		image.size += size
	}
}
