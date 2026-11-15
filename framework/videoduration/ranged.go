package videoduration

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ReadRanged reads the duration of the MP4 of size bytes behind r, reading only its header boxes
// (fork, TXE-7391). r is meant to be a remote file read with HTTP Range requests, where each ReadAt
// is a request: ReadRanged walks the top-level boxes by their headers (8 bytes, 16 for a 64-bit
// size), reads every box but mdat whole (ftyp, moov, every moof), and steps over each mdat by its
// size without reading a byte of its payload. A fragmented file spreads its moof boxes through the
// whole file; each is found this way. Read then runs over a sparse image of the file whose mdat
// payloads are zero, which it never reads.
//
// What is read is bounded by limits: the kept boxes by MaxHeaderBytes, the boxes visited by
// MaxBoxes, so a file costs at most MaxHeaderBytes plus 16 bytes and two reads per top-level box,
// whatever its size. A file beyond the limits, truncated or not an MP4 is ErrUnmeasurable. A read
// error of r other than io.EOF, and the context's error, are returned as they are.
func ReadRanged(ctx context.Context, r io.ReaderAt, size int64, limits ReadLimits) (Duration, error) {
	src := &videoSource{ctx: ctx, r: r}
	image, err := readTopLevelBoxesAt(src, size, limits)
	switch {
	case src.err != nil:
		return Duration{}, src.err
	case err != nil:
		return Duration{}, fmt.Errorf("%w: %w", ErrUnmeasurable, err)
	}
	return ReadWithLimits(ctx, image, image.size, limits)
}

// readTopLevelBoxesAt is readTopLevelBoxes for a file read at offsets: mdat payloads are stepped
// over, never read.
func readTopLevelBoxesAt(r io.ReaderAt, size int64, limits ReadLimits) (*sparseFile, error) {
	if size <= 0 {
		return nil, fmt.Errorf("%w: the file is empty", errVideoInvalid)
	}
	image := &sparseFile{size: size}
	var kept int64
	readFull := func(p []byte, off int64) error {
		n, err := r.ReadAt(p, off)
		if n == len(p) {
			return nil
		}
		if err == nil || errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	for off, boxes := int64(0), 0; off < size; boxes++ {
		if boxes >= limits.MaxBoxes {
			return nil, fmt.Errorf("%w: more than %d top-level boxes", errVideoReadLimit, limits.MaxBoxes)
		}
		if size-off < 8 {
			return nil, fmt.Errorf("%w: a box header cut short at byte %d", errVideoInvalid, off)
		}
		header := make([]byte, 8, 16)
		if err := readFull(header, off); err != nil {
			return nil, fmt.Errorf("%w: a box header at byte %d: %w", errVideoInvalid, off, err)
		}
		boxSize, typ := int64(binary.BigEndian.Uint32(header[:4])), string(header[4:8])
		switch boxSize {
		case 0:
			// The box runs to the end of the file, whose size is known here.
			boxSize = size - off
		case 1:
			if size-off < 16 {
				return nil, fmt.Errorf("%w: a box's 64-bit size cut short", errVideoInvalid)
			}
			header = header[:16]
			if err := readFull(header[8:16], off+8); err != nil {
				return nil, fmt.Errorf("%w: a box's 64-bit size at byte %d: %w", errVideoInvalid, off, err)
			}
			u := binary.BigEndian.Uint64(header[8:16])
			if u > uint64(size-off) {
				return nil, fmt.Errorf("%w: a %s box of %d bytes runs past the file's %d", errVideoInvalid, typ, u, size)
			}
			boxSize = int64(u)
		}
		headerLen := int64(len(header))
		if boxSize < headerLen || boxSize > size-off {
			return nil, fmt.Errorf("%w: a %s box of %d bytes at byte %d", errVideoInvalid, typ, boxSize, off)
		}
		if typ == "mdat" {
			// The media itself: step over it, keep its header so the box structure reads whole. A
			// size-0 header ("to the end") is given the size it reaches, as ReadStream does.
			if binary.BigEndian.Uint32(header[:4]) == 0 {
				if boxSize >= 1<<32 {
					return nil, fmt.Errorf("%w: an mdat to the end of the file past 4 GiB", errVideoReadLimit)
				}
				binary.BigEndian.PutUint32(header[:4], uint32(boxSize))
			}
			image.segments = append(image.segments, sparseSegment{offset: off, data: header})
			off += boxSize
			continue
		}
		payload := boxSize - headerLen
		// A container (moov of a long file) can hold more than MaxBoxBytes; Read holds each box it
		// decodes to that. What is kept is held to MaxHeaderBytes.
		if payload > limits.MaxHeaderBytes-kept {
			return nil, fmt.Errorf("%w: a %s box of %d bytes", errVideoReadLimit, typ, payload)
		}
		kept += payload
		box := make([]byte, boxSize)
		copy(box, header)
		if payload > 0 {
			if err := readFull(box[headerLen:], off+headerLen); err != nil {
				return nil, fmt.Errorf("%w: the %s box at byte %d: %w", errVideoInvalid, typ, off, err)
			}
		}
		image.segments = append(image.segments, sparseSegment{offset: off, data: box})
		off += boxSize
	}
	return image, nil
}
