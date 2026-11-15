package videoduration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	mp4 "github.com/abema/go-mp4"
)

// TestReadStreamMatchesRead: every fixture streamed reads as Read reads it held whole.
func TestReadStreamMatchesRead(t *testing.T) {
	for _, path := range []string{fixtureMoovLast, fixtureFaststart, fixtureSilent, fixtureFragmented} {
		data := readFixture(t, path)
		want, err := Read(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReadStream(context.Background(), bytes.NewReader(data), DefaultReadLimits, 1<<40)
		if err != nil || got != want {
			t.Fatalf("%s: got %+v, %v; want %+v", path, got, err, want)
		}
	}
}

// inflatedMoovLast is the moov-last fixture with extra zero bytes after the media in its mdat: the
// same 12-second clip, n bytes larger, streamed without the extra bytes ever being held. The samples'
// chunk offsets point into the mdat before the extra bytes, so they stay right; moov, after the mdat,
// moves along.
func inflatedMoovLast(t *testing.T, n int64) (io.Reader, int64) {
	t.Helper()
	data := readFixture(t, fixtureMoovLast)
	boxes, err := mp4.ExtractBox(bytes.NewReader(data), nil, mp4.BoxPath{mp4.BoxTypeMdat()})
	if err != nil || len(boxes) != 1 || boxes[0].HeaderSize != 8 {
		t.Fatalf("find mdat: %v %+v", err, boxes)
	}
	m := boxes[0]
	prefix := append([]byte(nil), data[:m.Offset+m.Size]...)
	binary.BigEndian.PutUint32(prefix[m.Offset:], uint32(m.Size+uint64(n)))
	return io.MultiReader(bytes.NewReader(prefix), io.LimitReader(zeros{}, n), bytes.NewReader(data[m.Offset+m.Size:])), int64(len(data)) + n
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// TestReadStreamMeasuresAFilePastTheBodyLimit is TXE-7334's second criterion: a 600 MB file, past the
// gateway's default 512 MB response body limit, is measured while holding its header only.
func TestReadStreamMeasuresAFilePastTheBodyLimit(t *testing.T) {
	r, size := inflatedMoovLast(t, 600<<20)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := ReadStream(context.Background(), r, DefaultReadLimits, 1<<40)
	runtime.ReadMemStats(&after)
	if err != nil || got.VideoMS != 12000 {
		t.Fatalf("a %d-byte file: got %+v, %v; want video 12000 ms", size, got, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("measuring a %d MiB file allocated %d MiB: the media is being held", size>>20, allocated>>20)
	}
}

// A file past maxBytes is unmeasurable; so is one cut short anywhere, mid-mdat included.
func TestReadStreamRefusesWhatItCannotMeasure(t *testing.T) {
	r, size := inflatedMoovLast(t, 1<<20)
	if _, err := ReadStream(context.Background(), r, DefaultReadLimits, size-1); !errors.Is(err, ErrUnmeasurable) || !errors.Is(err, errVideoReadLimit) {
		t.Fatalf("a file 1 byte past maxBytes: got %v, want a read-limit ErrUnmeasurable", err)
	}
	data := readFixture(t, fixtureMoovLast)
	for _, cut := range []int{len(data) - 1, len(data) - 200, len(data) / 2, 600, 40, 7} {
		if got, err := ReadStream(context.Background(), bytes.NewReader(data[:cut]), DefaultReadLimits, 1<<40); !errors.Is(err, ErrUnmeasurable) {
			t.Fatalf("first %d of %d bytes: got %+v, %v; want ErrUnmeasurable", cut, len(data), got, err)
		}
	}
	for name, limits := range map[string]ReadLimits{
		"boxes":        {MaxHeaderBytes: 32 << 20, MaxBoxBytes: 4 << 20, MaxBoxes: 2},
		"header bytes": {MaxHeaderBytes: 64, MaxBoxBytes: 4 << 20, MaxBoxes: 1 << 18},
	} {
		if _, err := ReadStream(context.Background(), bytes.NewReader(data), limits, 1<<40); !errors.Is(err, errVideoReadLimit) {
			t.Fatalf("%s: got %v, want a read-limit error", name, err)
		}
	}
}

// An mdat whose size field is 0 runs to the end of the file.
func TestReadStreamMdatToTheEnd(t *testing.T) {
	data := append([]byte(nil), readFixture(t, fixtureFaststart)...)
	boxes, err := mp4.ExtractBox(bytes.NewReader(data), nil, mp4.BoxPath{mp4.BoxTypeMdat()})
	if err != nil || len(boxes) != 1 || boxes[0].Offset+boxes[0].Size != uint64(len(data)) {
		t.Fatalf("the faststart fixture should end with its mdat: %v %+v", err, boxes)
	}
	binary.BigEndian.PutUint32(data[boxes[0].Offset:], 0)
	got, err := ReadStream(context.Background(), bytes.NewReader(data), DefaultReadLimits, 1<<40)
	if err != nil || got.VideoMS != 12000 {
		t.Fatalf("got %+v, %v; want video 12000 ms", got, err)
	}
}

// A failing read and a cancelled context are the caller's, never an unmeasurable input.
func TestReadStreamFailuresAreTheCallers(t *testing.T) {
	broken := errors.New("connection reset")
	data := readFixture(t, fixtureMoovLast)
	_, err := ReadStream(context.Background(), io.MultiReader(bytes.NewReader(data[:1000]), failing{broken}), DefaultReadLimits, 1<<40)
	if !errors.Is(err, broken) || errors.Is(err, ErrUnmeasurable) {
		t.Fatalf("got %v, want the read error alone", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ReadStream(ctx, bytes.NewReader(data), DefaultReadLimits, 1<<40)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnmeasurable) {
		t.Fatalf("got %v, want context.Canceled alone", err)
	}
}

// The sparse image reads held bytes as held and every gap as zero, across segment boundaries, and a
// read finds its segments by search: 100,000 reads of a 100,000-segment image take a moment, where a
// pass over every segment per read would take minutes.
func TestSparseFileReadAt(t *testing.T) {
	f := &sparseFile{size: 12, segments: []sparseSegment{{offset: 2, data: []byte("ab")}, {offset: 6, data: []byte("cde")}}}
	p := make([]byte, 12)
	if n, err := f.ReadAt(p, 0); n != 12 || err != nil || !bytes.Equal(p, []byte("\x00\x00ab\x00\x00cde\x00\x00\x00")) {
		t.Fatalf("got %d %v %q", n, err, p)
	}
	q := make([]byte, 4)
	if n, err := f.ReadAt(q, 3); n != 4 || err != nil || !bytes.Equal(q, []byte("b\x00\x00c")) {
		t.Fatalf("got %d %v %q", n, err, q)
	}
	if n, err := f.ReadAt(q, 10); n != 2 || err != io.EOF || !bytes.Equal(q[:2], []byte{0, 0}) {
		t.Fatalf("a read past the end: got %d %v", n, err)
	}

	const segments = 100_000
	big := &sparseFile{size: segments * 16}
	for i := range segments {
		big.segments = append(big.segments, sparseSegment{offset: int64(i) * 16, data: []byte{byte(i), 1, 2, 3, 4, 5, 6, 7}})
	}
	start := time.Now()
	b := make([]byte, 1)
	for i := range segments {
		if _, err := big.ReadAt(b, int64(i)*16); err != nil || b[0] != byte(i) {
			t.Fatalf("segment %d: got %d %v", i, b[0], err)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("%d reads of a %d-segment image took %v", segments, segments, elapsed)
	}
}

type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

// recordingReaderAt is a file read at offsets, as over HTTP Range requests: it records every read.
type recordingReaderAt struct {
	r     io.ReaderAt
	reads [][2]int64 // [offset, end)
	bytes int64
	err   error // returned by every read when set
}

func (r *recordingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.r.ReadAt(p, off)
	r.reads = append(r.reads, [2]int64{off, off + int64(len(p))})
	r.bytes += int64(n)
	return n, err
}

// TestReadRangedMatchesRead: every fixture read at offsets reads as Read reads it held whole, and no
// read asks for a byte of media: faststart (moov first), moov last, and fragmented (a moof before
// every mdat) alike (TXE-7391).
func TestReadRangedMatchesRead(t *testing.T) {
	for _, path := range []string{fixtureMoovLast, fixtureFaststart, fixtureSilent, fixtureFragmented} {
		data := readFixture(t, path)
		want, err := Read(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		rec := &recordingReaderAt{r: bytes.NewReader(data)}
		got, err := ReadRanged(context.Background(), rec, int64(len(data)), DefaultReadLimits)
		if err != nil || got != want {
			t.Fatalf("%s: got %+v, %v; want %+v", path, got, err, want)
		}
		for _, read := range rec.reads {
			for _, media := range mdatPayloads(t, data) {
				if read[0] < media[1] && media[0] < read[1] {
					t.Fatalf("%s: read [%d, %d) asks for media bytes [%d, %d)", path, read[0], read[1], media[0], media[1])
				}
			}
		}
		if mediaBytes := func() (n int64) {
			for _, m := range mdatPayloads(t, data) {
				n += m[1] - m[0]
			}
			return
		}(); rec.bytes > int64(len(data))-mediaBytes {
			t.Fatalf("%s: read %d bytes of a %d-byte file with %d media bytes", path, rec.bytes, len(data), mediaBytes)
		}
	}
}

// inflatedReaderAt is inflatedMoovLast read at offsets: n zero bytes of media are never stored.
type inflatedReaderAt struct {
	prefix, suffix []byte
	zeros          int64
}

func (f *inflatedReaderAt) size() int64 { return int64(len(f.prefix)) + f.zeros + int64(len(f.suffix)) }

func (f *inflatedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) && off+int64(n) < f.size() {
		at := off + int64(n)
		switch {
		case at < int64(len(f.prefix)):
			n += copy(p[n:], f.prefix[at:])
		case at < int64(len(f.prefix))+f.zeros:
			k := min(int64(len(p)-n), int64(len(f.prefix))+f.zeros-at)
			clear(p[n : n+int(k)])
			n += int(k)
		default:
			n += copy(p[n:], f.suffix[at-int64(len(f.prefix))-f.zeros:])
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// TestReadRangedReadsOnlyTheHeaderOfALargeFile: a moov-last file whose mdat runs to 6 GiB (a 64-bit
// box size) is measured from a few kilobytes, its media never read.
func TestReadRangedReadsOnlyTheHeaderOfALargeFile(t *testing.T) {
	data := readFixture(t, fixtureMoovLast)
	boxes, err := mp4.ExtractBox(bytes.NewReader(data), nil, mp4.BoxPath{mp4.BoxTypeMdat()})
	if err != nil || len(boxes) != 1 || boxes[0].HeaderSize != 8 {
		t.Fatalf("find mdat: %v %+v", err, boxes)
	}
	m := boxes[0]
	// The mdat is rewritten with a 64-bit size, 8 bytes longer, so the chunk offsets in moov move
	// by 8: the clip is measured from moov's timing, which does not depend on them, but checkChunks
	// would refuse offsets outside mdat, so the media is padded at the end, after the samples.
	const extra = 6 << 30
	head := append([]byte(nil), data[:m.Offset]...)
	large := make([]byte, 16)
	binary.BigEndian.PutUint32(large[0:4], 1)
	copy(large[4:8], "mdat")
	binary.BigEndian.PutUint64(large[8:16], m.Size+8+extra)
	head = append(head, large...)
	head = append(head, data[m.Offset+8:m.Offset+m.Size]...)
	f := &inflatedReaderAt{prefix: head, zeros: extra, suffix: shiftChunkOffsets(t, data[m.Offset+m.Size:], 8)}
	rec := &recordingReaderAt{r: f}
	got, err := ReadRanged(context.Background(), rec, f.size(), DefaultReadLimits)
	if err != nil || got.VideoMS != 12000 {
		t.Fatalf("a %d-byte file: got %+v, %v; want video 12000 ms", f.size(), got, err)
	}
	if rec.bytes > 64<<10 {
		t.Fatalf("measuring a %d GiB file read %d bytes", f.size()>>30, rec.bytes)
	}
}

// shiftChunkOffsets returns moov with every stco entry moved by delta bytes.
func shiftChunkOffsets(t *testing.T, moov []byte, delta uint32) []byte {
	t.Helper()
	out := append([]byte(nil), moov...)
	boxes, err := mp4.ExtractBox(bytes.NewReader(out), nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStco()})
	if err != nil || len(boxes) == 0 {
		t.Fatalf("find stco: %v %+v", err, boxes)
	}
	for _, b := range boxes {
		// full box header (4), entry count (4), then 4-byte offsets.
		p := out[b.Offset+b.HeaderSize:]
		count := binary.BigEndian.Uint32(p[4:8])
		for i := uint32(0); i < count; i++ {
			at := 8 + 4*i
			binary.BigEndian.PutUint32(p[at:], binary.BigEndian.Uint32(p[at:])+delta)
		}
	}
	return out
}

// A file cut short is unmeasurable, and a read error is the caller's, returned as it is.
func TestReadRangedRefusesWhatItCannotMeasure(t *testing.T) {
	data := readFixture(t, fixtureMoovLast)
	for _, cut := range []int{len(data) - 1, len(data) - 200, len(data) / 2, 600, 40, 7} {
		// The size is the whole file's, as a Content-Range total says, but the bytes stop at cut.
		if got, err := ReadRanged(context.Background(), bytes.NewReader(data[:cut]), int64(len(data)), DefaultReadLimits); !errors.Is(err, ErrUnmeasurable) {
			t.Fatalf("first %d of %d bytes: got %+v, %v; want ErrUnmeasurable", cut, len(data), got, err)
		}
	}
	if _, err := ReadRanged(context.Background(), bytes.NewReader(data), 0, DefaultReadLimits); !errors.Is(err, ErrUnmeasurable) {
		t.Fatalf("an empty file: got %v, want ErrUnmeasurable", err)
	}
	boom := errors.New("range request failed")
	if _, err := ReadRanged(context.Background(), &recordingReaderAt{err: boom}, int64(len(data)), DefaultReadLimits); !errors.Is(err, boom) || errors.Is(err, ErrUnmeasurable) {
		t.Fatalf("a failing read: got %v, want the read's own error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadRanged(ctx, bytes.NewReader(data), int64(len(data)), DefaultReadLimits); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context: got %v, want context.Canceled", err)
	}
	tight := DefaultReadLimits
	tight.MaxHeaderBytes = 100
	if _, err := ReadRanged(context.Background(), bytes.NewReader(data), int64(len(data)), tight); !errors.Is(err, errVideoReadLimit) {
		t.Fatalf("a header past MaxHeaderBytes: got %v, want a read-limit ErrUnmeasurable", err)
	}
}
