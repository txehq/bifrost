package videoduration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"

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

type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }
