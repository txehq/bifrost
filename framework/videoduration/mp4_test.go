package videoduration

// These are TXE-6774's MP4 reader tests (tokun/internal/mediabound/video_test.go in txehq/txe),
// carried over with the reader; the WebM and metering-rule tests stay there.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

// The fixtures are 12 seconds of ffmpeg's testsrc at 10 frames per second with a 16 kHz sine track,
// written by testdata/generate.sh. Their durations as the headers state them, in ticks, are
// checked against ffprobe's duration_ts.
const (
	fixtureMoovLast   = "testdata/twelve-seconds.mp4"
	fixtureFaststart  = "testdata/twelve-seconds-faststart.mp4"
	fixtureSilent     = "testdata/twelve-seconds-silent.mp4"
	fixtureFragmented = "testdata/twelve-seconds-fragmented.mp4"
)

// rangeCounter is an io.ReaderAt over a file's bytes that records every range read from it.
type rangeCounter struct {
	data   []byte
	ranges [][2]int64
	bytes  int64
}

func (c *rangeCounter) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(c.data)) {
		return 0, io.EOF
	}
	n := copy(p, c.data[off:])
	c.ranges = append(c.ranges, [2]int64{off, off + int64(n)})
	c.bytes += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// overlaps reports whether any range read overlaps [start, end).
func (c *rangeCounter) overlaps(start, end int64) bool {
	for _, r := range c.ranges {
		if r[0] < end && start < r[1] {
			return true
		}
	}
	return false
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// mdatPayloads are the byte ranges of an MP4's media data.
func mdatPayloads(t *testing.T, data []byte) [][2]int64 {
	t.Helper()
	boxes, err := mp4.ExtractBox(bytes.NewReader(data), nil, mp4.BoxPath{mp4.BoxTypeMdat()})
	if err != nil || len(boxes) == 0 {
		t.Fatalf("find mdat: %v (%d boxes)", err, len(boxes))
	}
	var out [][2]int64
	for _, b := range boxes {
		out = append(out, [2]int64{int64(b.Offset + b.HeaderSize), int64(b.Offset + b.Size)})
	}
	return out
}

// TestTwelveSecondMP4ReportsTwelveSecondsFromItsHeader is the ticket's first criterion: a 12-second MP4
// reports 12 s, and the range reads never touch its media data, wherever moov sits.
func TestTwelveSecondMP4ReportsTwelveSecondsFromItsHeader(t *testing.T) {
	cases := []struct {
		path             string
		videoMS, audioMS int64
	}{
		// mdhd: video 122,880 ticks at 10,240 per second; audio 193,024 samples at 16,000 per second,
		// the 1,024 priming samples of AAC included (ffprobe shows 192,000 after the edit list).
		{path: fixtureMoovLast, videoMS: 12000, audioMS: 12064},
		{path: fixtureFaststart, videoMS: 12000, audioMS: 12064},
		{path: fixtureSilent, videoMS: 12000, audioMS: 0},
		// An empty moov and six 2-second fragments: the duration is the last fragment's end, 123,536
		// video ticks (12.0640625 s, ffprobe's duration_ts) and 193,024 audio samples.
		{path: fixtureFragmented, videoMS: 12065, audioMS: 12064},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			data := readFixture(t, c.path)
			r := &rangeCounter{data: data}
			got, err := Read(context.Background(), r, int64(len(data)))
			if err != nil {
				t.Fatalf("ReadVideoDuration: %v", err)
			}
			want := Duration{Container: ContainerMP4, VideoMS: c.videoMS, AudioMS: c.audioMS}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			for _, m := range mdatPayloads(t, data) {
				if r.overlaps(m[0], m[1]) {
					t.Fatalf("a read overlaps mdat's payload [%d, %d): reads %v", m[0], m[1], r.ranges)
				}
			}
			if r.bytes >= int64(len(data))/2 {
				t.Fatalf("read %d of the file's %d bytes", r.bytes, len(data))
			}
		})
	}
}

// TestEditListsBoundThePresentedDuration checks that an edit list presenting more than the media holds
// (a repeated span, an empty edit before it) raises the duration to what it presents, in the movie's
// timescale, and that one presenting less leaves the media's duration.
func TestEditListsBoundThePresentedDuration(t *testing.T) {
	cases := []struct {
		name   string
		movie  uint32
		edits  []mp4.ElstEntry
		wantMS int64
		bad    bool
	}{
		{name: "the 12-second span played twice", movie: 600,
			edits: []mp4.ElstEntry{{SegmentDurationV0: 7200, MediaRateInteger: 1}, {SegmentDurationV0: 7200, MediaRateInteger: 1}}, wantMS: 24000},
		{name: "an empty edit of 3 s before the media", movie: 1000,
			edits: []mp4.ElstEntry{{SegmentDurationV0: 3000, MediaTimeV0: -1, MediaRateInteger: 1}, {SegmentDurationV0: 12000, MediaRateInteger: 1}}, wantMS: 15000},
		{name: "an edit shorter than the media keeps the media's", movie: 1000,
			edits: []mp4.ElstEntry{{SegmentDurationV0: 6000, MediaRateInteger: 1}}, wantMS: 12000},
		{name: "an edit list with no movie timescale", movie: 0,
			edits: []mp4.ElstEntry{{SegmentDurationV0: 6000, MediaRateInteger: 1}}, bad: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := buildMP4(t, func(w *mp4.Writer) {
				chunk := mediaData(t, w, 100)
				box(t, w, mp4.BoxTypeMoov(), func() {
					leaf(t, w, &mp4.Mvhd{Timescale: c.movie})
					editedVideoTrak(t, w, 90000, 12*90000, &mp4.Elst{EntryCount: uint32(len(c.edits)), Entries: c.edits}, chunk)
				})
			})
			got, err := Read(context.Background(), &rangeCounter{data: data}, int64(len(data)))
			if c.bad {
				if !errors.Is(err, ErrUnmeasurable) {
					t.Fatalf("got %+v, %v; want ErrUnmeasurableInput", got, err)
				}
				return
			}
			if err != nil || got.VideoMS != c.wantMS {
				t.Fatalf("got %+v, %v; want video %d ms", got, err, c.wantMS)
			}
		})
	}
}

// TestTimingTablesBoundTheMediaDuration checks that the timing table, not a shortened mdhd, decides how
// long the media lasts: stts's deltas, plus the largest composition offset in ctts; and that an stts
// timing another number of samples than stsz sizes is unmeasurable.
func TestTimingTablesBoundTheMediaDuration(t *testing.T) {
	build := func(mdhdTicks, sttsSamples uint32, ctts *mp4.Ctts) []byte {
		return buildMP4(t, func(w *mp4.Writer) {
			at := mediaData(t, w, 120)
			box(t, w, mp4.BoxTypeMoov(), func() {
				leaf(t, w, &mp4.Mvhd{Timescale: 1000})
				box(t, w, mp4.BoxTypeTrak(), func() {
					leaf(t, w, &mp4.Tkhd{TrackID: 1})
					box(t, w, mp4.BoxTypeMdia(), func() {
						leaf(t, w, &mp4.Mdhd{Timescale: 1000, DurationV0: mdhdTicks})
						leaf(t, w, &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}})
						box(t, w, mp4.BoxTypeMinf(), func() {
							box(t, w, mp4.BoxTypeStbl(), func() {
								// 12 samples of 1,000 ticks: 12 s.
								leaf(t, w, &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: sttsSamples, SampleDelta: 1000}}})
								if ctts != nil {
									leaf(t, w, ctts)
								}
								leaf(t, w, &mp4.Stsz{SampleSize: 10, SampleCount: 12})
								leaf(t, w, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: 12, SampleDescriptionIndex: 1}}})
								leaf(t, w, &mp4.Stco{EntryCount: 1, ChunkOffset: []uint32{at}})
							})
						})
					})
				})
			})
		})
	}
	offsets := &mp4.Ctts{EntryCount: 2, Entries: []mp4.CttsEntry{{SampleCount: 11, SampleOffsetV0: 500}, {SampleCount: 1, SampleOffsetV0: 2000}}}
	reordered := &mp4.Ctts{FullBox: mp4.FullBox{Version: 1}, EntryCount: 4, Entries: []mp4.CttsEntry{
		{SampleCount: 1, SampleOffsetV1: 0}, {SampleCount: 1, SampleOffsetV1: 2000}, {SampleCount: 2, SampleOffsetV1: -1000}, {SampleCount: 8, SampleOffsetV1: 0},
	}}
	early := &mp4.Ctts{FullBox: mp4.FullBox{Version: 1}, EntryCount: 1, Entries: []mp4.CttsEntry{{SampleCount: 12, SampleOffsetV1: -10000}}}
	for name, c := range map[string]struct {
		data   []byte
		wantMS int64
	}{
		"mdhd shortened to 1 s":             {data: build(1000, 12, nil), wantMS: 12000},
		"mdhd longer than the samples":      {data: build(15000, 12, nil), wantMS: 15000},
		"a composition offset past the end": {data: build(12000, 12, offsets), wantMS: 14000},
		// TXE-7334: the second sample presents 2 s late, until 4 s, and the two after it 1 s early; the
		// last sample still ends the clip at 12 s. The largest offset added to the decode end gave 14 s.
		"a reordered sample does not lengthen the clip": {data: build(1000, 12, reordered), wantMS: 12000},
		// Offsets that pull every sample 10 s early do not make 12 s of samples shorter than 12 s.
		"negative offsets do not shorten the clip": {data: build(1000, 12, early), wantMS: 12000},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Read(context.Background(), &rangeCounter{data: c.data}, int64(len(c.data)))
			if err != nil || got.VideoMS != c.wantMS {
				t.Fatalf("got %+v, %v; want video %d ms", got, err, c.wantMS)
			}
		})
	}
	bad := build(12000, 11, nil)
	if _, err := Read(context.Background(), &rangeCounter{data: bad}, int64(len(bad))); !errors.Is(err, ErrUnmeasurable) {
		t.Fatalf("stts timing 11 of 12 samples: got %v, want ErrUnmeasurableInput", err)
	}
}

// TestFragmentCompositionOffsetsExtendTheDuration checks that a fragment run's composition offsets
// count, sample by sample: the three samples present until 4, 9.5 and 12.5 seconds, so the clip lasts
// 12.5 s. Adding the largest offset to the run's decode end gave 13.5 s (TXE-7334).
func TestFragmentCompositionOffsetsExtendTheDuration(t *testing.T) {
	data := buildMP4(t, func(w *mp4.Writer) {
		box(t, w, mp4.BoxTypeMoov(), func() {
			leaf(t, w, &mp4.Mvhd{Timescale: 1000})
			videoTrak(t, w, 1, 1000, 0)
			box(t, w, mp4.BoxTypeMvex(), func() { leaf(t, w, &mp4.Trex{TrackID: 1, DefaultSampleDuration: 4000}) })
		})
		box(t, w, mp4.BoxTypeMoof(), func() {
			box(t, w, mp4.BoxTypeTraf(), func() {
				leaf(t, w, &mp4.Tfhd{TrackID: 1})
				// three samples of 4,000 ticks, composition offsets 0, 1,500 and 500.
				raw(t, w, mp4.BoxTypeTrun(), trunBytes(0x000800, 3, 0, 1500, 500))
			})
		})
	})
	got, err := Read(context.Background(), &rangeCounter{data: data}, int64(len(data)))
	if err != nil || got.VideoMS != 12500 {
		t.Fatalf("got %+v, %v; want video 12500 ms", got, err)
	}
}

// TestEditListCountPastItsPayloadIsUnmeasurable checks that an elst declaring more entries than its
// payload holds is refused.
func TestEditListCountPastItsPayloadIsUnmeasurable(t *testing.T) {
	data := buildMP4(t, func(w *mp4.Writer) {
		box(t, w, mp4.BoxTypeMoov(), func() {
			leaf(t, w, &mp4.Mvhd{Timescale: 1000})
			box(t, w, mp4.BoxTypeTrak(), func() {
				leaf(t, w, &mp4.Tkhd{TrackID: 1})
				box(t, w, mp4.BoxTypeEdts(), func() {
					// version 0, flags 0, 0xffffffff entries, one 12-byte entry.
					p := binary.BigEndian.AppendUint32(nil, 0)
					p = binary.BigEndian.AppendUint32(p, 0xffffffff)
					p = binary.BigEndian.AppendUint32(p, 12000)
					p = binary.BigEndian.AppendUint32(p, 0)
					p = binary.BigEndian.AppendUint32(p, 1<<16)
					raw(t, w, mp4.BoxTypeElst(), p)
				})
				box(t, w, mp4.BoxTypeMdia(), func() {
					leaf(t, w, &mp4.Mdhd{Timescale: 1000, DurationV0: 12000})
					leaf(t, w, &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}})
				})
			})
		})
	})
	_, err := Read(context.Background(), &rangeCounter{data: data}, int64(len(data)))
	if !errors.Is(err, ErrUnmeasurable) {
		t.Fatalf("got %v, want ErrUnmeasurableInput", err)
	}
}

// TestMP4CutAtABoxBoundaryIsUnmeasurable cuts each fixture where a top-level box starts, so every box
// left ends within the file. A cut that leaves samples whose data is not there (before or between the
// mdats, or a moof without its mdat) or leaves no moov is unmeasurable. A fragmented file cut before a
// moof is a shorter file whose every sample is there: it measures the fragments left, more with each one
// and less than the whole. The trailing mfra is an index and may go.
func TestMP4CutAtABoxBoundaryIsUnmeasurable(t *testing.T) {
	for _, path := range []string{fixtureMoovLast, fixtureFaststart, fixtureSilent, fixtureFragmented} {
		data := readFixture(t, path)
		full, err := Read(context.Background(), &rangeCounter{data: data}, int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		top, err := mp4.ExtractBoxes(bytes.NewReader(data), nil, []mp4.BoxPath{{mp4.BoxTypeAny()}})
		if err != nil {
			t.Fatal(err)
		}
		var previous int64
		for _, b := range top[1:] {
			cut := int(b.Offset)
			got, err := Read(context.Background(), &rangeCounter{data: data[:cut]}, int64(cut))
			switch {
			case b.Type == mp4.BoxTypeMfra():
				if err != nil || got != full {
					t.Fatalf("%s without its mfra: got %+v, %v; want %+v", path, got, err, full)
				}
			case b.Type == mp4.BoxTypeMoof() && previous == 0:
				// The first moof: the empty moov alone holds no samples.
				if !errors.Is(err, ErrUnmeasurable) {
					t.Fatalf("%s cut at its first moof: got %+v, %v; want ErrUnmeasurableInput", path, got, err)
				}
				previous = -1
			case b.Type == mp4.BoxTypeMoof():
				if err != nil || got.VideoMS <= previous || got.VideoMS >= full.VideoMS {
					t.Fatalf("%s cut at the moof at byte %d: got %+v, %v; want more than %d ms and less than %d", path, cut, got, err, previous, full.VideoMS)
				}
				previous = got.VideoMS
			default:
				if !errors.Is(err, ErrUnmeasurable) {
					t.Fatalf("%s cut at %s (byte %d): got %+v, %v; want ErrUnmeasurableInput", path, b.Type, cut, got, err)
				}
			}
		}
	}
}

// TestSampleDataOutsideAnMdatIsUnmeasurable checks chunks and fragment runs whose data lies past the
// mdat they should be in, or in none.
func TestSampleDataOutsideAnMdatIsUnmeasurable(t *testing.T) {
	trak := func(w *mp4.Writer, offsets []uint32, perChunk, size uint32) {
		box(t, w, mp4.BoxTypeTrak(), func() {
			leaf(t, w, &mp4.Tkhd{TrackID: 1})
			box(t, w, mp4.BoxTypeMdia(), func() {
				leaf(t, w, &mp4.Mdhd{Timescale: 1000, DurationV0: 12000})
				leaf(t, w, &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}})
				sampleTable(t, w, offsets, perChunk, size, 12000/(perChunk*uint32(len(offsets))))
			})
		})
	}
	whole := buildMP4(t, func(w *mp4.Writer) {
		at := mediaData(t, w, 100)
		box(t, w, mp4.BoxTypeMoov(), func() { leaf(t, w, &mp4.Mvhd{Timescale: 1000}); trak(w, []uint32{at, at + 50}, 2, 25) })
	})
	if got, err := Read(context.Background(), &rangeCounter{data: whole}, int64(len(whole))); err != nil || got.VideoMS != 12000 {
		t.Fatalf("two chunks filling their mdat: got %+v, %v; want video 12000 ms", got, err)
	}
	for name, build := range map[string]func(*mp4.Writer){
		"a chunk running past its mdat": func(w *mp4.Writer) {
			at := mediaData(t, w, 100)
			box(t, w, mp4.BoxTypeMoov(), func() { leaf(t, w, &mp4.Mvhd{Timescale: 1000}); trak(w, []uint32{at + 60}, 2, 25) })
		},
		"a chunk in no mdat": func(w *mp4.Writer) {
			box(t, w, mp4.BoxTypeMoov(), func() { leaf(t, w, &mp4.Mvhd{Timescale: 1000}); trak(w, []uint32{8}, 1, 25) })
		},
		"a fragment run with no mdat after its moof": func(w *mp4.Writer) {
			box(t, w, mp4.BoxTypeMoov(), func() {
				leaf(t, w, &mp4.Mvhd{Timescale: 1000})
				videoTrak(t, w, 1, 1000, 0)
				box(t, w, mp4.BoxTypeMvex(), func() {
					leaf(t, w, &mp4.Trex{TrackID: 1, DefaultSampleDuration: 1000, DefaultSampleSize: 10})
				})
			})
			box(t, w, mp4.BoxTypeMoof(), func() {
				box(t, w, mp4.BoxTypeTraf(), func() {
					leaf(t, w, &mp4.Tfhd{TrackID: 1})
					raw(t, w, mp4.BoxTypeTrun(), trunBytes(0, 12))
				})
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := buildMP4(t, build)
			_, err := Read(context.Background(), &rangeCounter{data: data}, int64(len(data)))
			if !errors.Is(err, ErrUnmeasurable) {
				t.Fatalf("got %v, want ErrUnmeasurableInput", err)
			}
		})
	}
}

// TestTruncatedVideoIsUnmeasurable is the ticket's second criterion: a video cut short anywhere, in its
// media or in its header, is unmeasurable_input, even when its moov is whole.
func TestTruncatedVideoIsUnmeasurable(t *testing.T) {
	for _, path := range []string{fixtureMoovLast, fixtureFaststart, fixtureSilent, fixtureFragmented} {
		data := readFixture(t, path)
		for _, cut := range []int{len(data) - 1, len(data) - 200, len(data) / 2, 600, 40, 7} {
			t.Run(path, func(t *testing.T) {
				r := &rangeCounter{data: data[:cut]}
				got, err := Read(context.Background(), r, int64(cut))
				if !errors.Is(err, ErrUnmeasurable) {
					t.Fatalf("first %d of %d bytes: got %+v, %v; want ErrUnmeasurableInput", cut, len(data), got, err)
				}
			})
		}
	}
}

// TestNotAVideoIsUnmeasurable checks that an empty file, text, a PDF and an MP4 with no track are each
// unmeasurable_input.
func TestNotAVideoIsUnmeasurable(t *testing.T) {
	noTracks := buildMP4(t, func(w *mp4.Writer) {
		box(t, w, mp4.BoxTypeMoov(), func() { leaf(t, w, &mp4.Mvhd{Timescale: 1000, DurationV0: 12000}) })
	})
	for name, data := range map[string][]byte{
		"empty":     {},
		"text":      []byte("this is not a video, only some text"),
		"pdf":       []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj << >> endobj\n"),
		"no tracks": noTracks,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Read(context.Background(), &rangeCounter{data: data}, int64(len(data)))
			if !errors.Is(err, ErrUnmeasurable) {
				t.Fatalf("got %v, want ErrUnmeasurableInput", err)
			}
		})
	}
}

// TestReadFailuresAreTheCallers checks that a failing read and a cancelled context are returned as they
// are, never as an unmeasurable input the customer is told about.
func TestReadFailuresAreTheCallers(t *testing.T) {
	data := readFixture(t, fixtureMoovLast)
	broken := errors.New("object store connection reset")
	_, err := Read(context.Background(), failingReader{data: data, after: 64, err: broken}, int64(len(data)))
	if !errors.Is(err, broken) || errors.Is(err, ErrUnmeasurable) {
		t.Fatalf("got %v, want the read error alone", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Read(ctx, &rangeCounter{data: data}, int64(len(data)))
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnmeasurable) {
		t.Fatalf("got %v, want context.Canceled alone", err)
	}
}

// failingReader serves data until an offset, then fails every read with err.
type failingReader struct {
	data  []byte
	after int64
	err   error
}

func (f failingReader) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.after {
		return 0, f.err
	}
	return copy(p, f.data[off:]), nil
}

// TestVideoReadLimitsHold checks that a header needing more boxes or bytes than the limits allow is
// unmeasurable, for a fragmented and a faststart MP4 alike.
func TestVideoReadLimitsHold(t *testing.T) {
	for _, path := range []string{fixtureFragmented, fixtureFaststart} {
		data := readFixture(t, path)
		for name, limits := range map[string]ReadLimits{
			"boxes":        {MaxHeaderBytes: 32 << 20, MaxBoxBytes: 4 << 20, MaxBoxes: 8},
			"header bytes": {MaxHeaderBytes: 64, MaxBoxBytes: 4 << 20, MaxBoxes: 1 << 18},
			"box bytes":    {MaxHeaderBytes: 32 << 20, MaxBoxBytes: 8, MaxBoxes: 1 << 18},
		} {
			t.Run(path+" "+name, func(t *testing.T) {
				_, err := ReadWithLimits(context.Background(), &rangeCounter{data: data}, int64(len(data)), limits)
				if !errors.Is(err, ErrUnmeasurable) || !errors.Is(err, errVideoReadLimit) {
					t.Fatalf("got %v, want a read-limit ErrUnmeasurableInput", err)
				}
			})
		}
	}
}

// TestFragmentSampleDurations checks each source of a fragment's sample durations on built files: the
// trun's own, the tfhd's default, the trex's default, and none at all; and that a trun claiming more
// samples than its payload holds is refused without decoding them.
func TestFragmentSampleDurations(t *testing.T) {
	const timescale = 1000
	cases := []struct {
		name   string
		trex   uint32
		tfhd   *uint32
		trun   []byte
		wantMS int64
		bad    bool
	}{
		{name: "the trun's own durations", trun: trunBytes(0x000100, 3, 4000, 4000, 4500), wantMS: 12500},
		{name: "the tfhd's default", tfhd: u32(3000), trun: trunBytes(0, 4), wantMS: 12000},
		{name: "the trex's default", trex: 2000, trun: trunBytes(0, 6), wantMS: 12000},
		{name: "a crafted count with no per-sample fields multiplies the default", trex: 1, trun: trunBytes(0, 0xffffffff), wantMS: 0xffffffff},
		{name: "no default at all", trun: trunBytes(0, 6), bad: true},
		{name: "a count past the payload", trun: trunBytes(0x000100, 0xffffffff, 4000), bad: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := buildMP4(t, func(w *mp4.Writer) {
				box(t, w, mp4.BoxTypeMoov(), func() {
					leaf(t, w, &mp4.Mvhd{Timescale: timescale})
					videoTrak(t, w, 1, timescale, 0)
					box(t, w, mp4.BoxTypeMvex(), func() { leaf(t, w, &mp4.Trex{TrackID: 1, DefaultSampleDuration: c.trex}) })
				})
				box(t, w, mp4.BoxTypeMoof(), func() {
					box(t, w, mp4.BoxTypeTraf(), func() {
						tfhd := &mp4.Tfhd{TrackID: 1}
						if c.tfhd != nil {
							tfhd.AddFlag(mp4.TfhdDefaultSampleDurationPresent)
							tfhd.DefaultSampleDuration = *c.tfhd
						}
						leaf(t, w, tfhd)
						raw(t, w, mp4.BoxTypeTrun(), c.trun)
					})
				})
			})
			got, err := Read(context.Background(), &rangeCounter{data: data}, int64(len(data)))
			if c.bad {
				if !errors.Is(err, ErrUnmeasurable) {
					t.Fatalf("got %+v, %v; want ErrUnmeasurableInput", got, err)
				}
				return
			}
			if err != nil || got.VideoMS != c.wantMS {
				t.Fatalf("got %+v, %v; want video %d ms", got, err, c.wantMS)
			}
		})
	}
}

// TestFragmentsFollowTheirDecodeTime checks that a tfdt sets where a fragment starts, so a fragment
// list that restarts does not add up past the media's end.
func TestFragmentsFollowTheirDecodeTime(t *testing.T) {
	data := buildMP4(t, func(w *mp4.Writer) {
		box(t, w, mp4.BoxTypeMoov(), func() {
			leaf(t, w, &mp4.Mvhd{Timescale: 1000})
			videoTrak(t, w, 1, 1000, 0)
			box(t, w, mp4.BoxTypeMvex(), func() { leaf(t, w, &mp4.Trex{TrackID: 1, DefaultSampleDuration: 1000}) })
		})
		for _, start := range []uint32{0, 6000, 0} {
			box(t, w, mp4.BoxTypeMoof(), func() {
				box(t, w, mp4.BoxTypeTraf(), func() {
					leaf(t, w, &mp4.Tfhd{TrackID: 1})
					leaf(t, w, &mp4.Tfdt{BaseMediaDecodeTimeV0: start})
					raw(t, w, mp4.BoxTypeTrun(), trunBytes(0, 6))
				})
			})
		}
	})
	got, err := Read(context.Background(), &rangeCounter{data: data}, int64(len(data)))
	if err != nil || got.VideoMS != 12000 {
		t.Fatalf("got %+v, %v; want video 12000 ms", got, err)
	}
}

// TestInconsistentMP4IsUnmeasurable checks the structural refusals: a moof before moov, a fragment of a
// track moov does not hold, an mdia outside a trak, two traks with one id, and a trak without its mdhd.
func TestInconsistentMP4IsUnmeasurable(t *testing.T) {
	fragment := func(w *mp4.Writer, track uint32) {
		box(t, w, mp4.BoxTypeMoof(), func() {
			box(t, w, mp4.BoxTypeTraf(), func() {
				leaf(t, w, &mp4.Tfhd{TrackID: track})
				raw(t, w, mp4.BoxTypeTrun(), trunBytes(0x000100, 1, 1000))
			})
		})
	}
	moov := func(w *mp4.Writer, traks func()) {
		box(t, w, mp4.BoxTypeMoov(), func() { leaf(t, w, &mp4.Mvhd{Timescale: 1000}); traks() })
	}
	for name, build := range map[string]func(*mp4.Writer){
		"moof before moov": func(w *mp4.Writer) {
			fragment(w, 1)
			moov(w, func() { videoTrak(t, w, 1, 1000, 12000) })
		},
		"fragment of an unknown track": func(w *mp4.Writer) {
			moov(w, func() { videoTrak(t, w, 1, 1000, 12000) })
			fragment(w, 2)
		},
		"mdia outside a trak": func(w *mp4.Writer) {
			moov(w, func() {
				box(t, w, mp4.BoxTypeMdia(), func() { leaf(t, w, &mp4.Mdhd{Timescale: 1000, DurationV0: 12000}) })
			})
		},
		"mdia at the top level": func(w *mp4.Writer) {
			box(t, w, mp4.BoxTypeMdia(), func() { leaf(t, w, &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}}) })
			moov(w, func() { videoTrak(t, w, 1, 1000, 12000) })
		},
		"two traks with one id": func(w *mp4.Writer) {
			moov(w, func() { videoTrak(t, w, 1, 1000, 12000); videoTrak(t, w, 1, 1000, 12000) })
		},
		"trak without mdhd": func(w *mp4.Writer) {
			moov(w, func() {
				box(t, w, mp4.BoxTypeTrak(), func() {
					leaf(t, w, &mp4.Tkhd{TrackID: 1})
					box(t, w, mp4.BoxTypeMdia(), func() { leaf(t, w, &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}}) })
				})
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := buildMP4(t, build)
			_, err := Read(context.Background(), &rangeCounter{data: data}, int64(len(data)))
			if !errors.Is(err, ErrUnmeasurable) {
				t.Fatalf("got %v, want ErrUnmeasurableInput", err)
			}
		})
	}
}

// TestGeminiVideoRuleBoundsSixtySeconds is the ticket's third criterion: Gemini's rule bounds a
func u32(n uint32) *uint32 { return &n }

// memFile is an in-memory io.WriteSeeker for go-mp4's Writer.
type memFile struct {
	data []byte
	pos  int64
}

func (m *memFile) Write(p []byte) (int, error) {
	if end := m.pos + int64(len(p)); end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	n := copy(m.data[m.pos:], p)
	m.pos += int64(n)
	return n, nil
}

func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = offset
	case io.SeekCurrent:
		m.pos += offset
	case io.SeekEnd:
		m.pos = int64(len(m.data)) + offset
	}
	return m.pos, nil
}

// buildMP4 writes an ftyp, then whatever build writes, with go-mp4's Writer.
func buildMP4(t *testing.T, build func(*mp4.Writer)) []byte {
	t.Helper()
	f := &memFile{}
	w := mp4.NewWriter(f)
	leaf(t, w, &mp4.Ftyp{MajorBrand: [4]byte{'i', 's', 'o', 'm'}, CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'i', 's', 'o', '6'}}}})
	build(w)
	return f.data
}

func box(t *testing.T, w *mp4.Writer, typ mp4.BoxType, children func()) {
	t.Helper()
	if _, err := w.StartBox(&mp4.BoxInfo{Type: typ}); err != nil {
		t.Fatal(err)
	}
	children()
	if _, err := w.EndBox(); err != nil {
		t.Fatal(err)
	}
}

func leaf(t *testing.T, w *mp4.Writer, b mp4.IBox) {
	t.Helper()
	box(t, w, b.GetType(), func() {
		if _, err := mp4.Marshal(w, b, mp4.Context{}); err != nil {
			t.Fatal(err)
		}
	})
}

// raw writes a box of typ whose payload is p, for a trun whose sample count its payload does not hold.
func raw(t *testing.T, w *mp4.Writer, typ mp4.BoxType, p []byte) {
	t.Helper()
	box(t, w, typ, func() {
		if _, err := w.Write(p); err != nil {
			t.Fatal(err)
		}
	})
}

// trunBytes is a version 0 trun payload with flags, a sample count and one 4-byte field per value.
func trunBytes(flags uint32, count uint32, durations ...uint32) []byte {
	p := binary.BigEndian.AppendUint32(nil, flags)
	p = binary.BigEndian.AppendUint32(p, count)
	for _, d := range durations {
		p = binary.BigEndian.AppendUint32(p, d)
	}
	return p
}

func editedVideoTrak(t *testing.T, w *mp4.Writer, timescale, duration uint32, elst *mp4.Elst, chunk uint32) {
	t.Helper()
	box(t, w, mp4.BoxTypeTrak(), func() {
		leaf(t, w, &mp4.Tkhd{TrackID: 1})
		box(t, w, mp4.BoxTypeEdts(), func() { leaf(t, w, elst) })
		box(t, w, mp4.BoxTypeMdia(), func() {
			leaf(t, w, &mp4.Mdhd{Timescale: timescale, DurationV0: duration})
			leaf(t, w, &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}})
			sampleTable(t, w, []uint32{chunk}, 4, 25, duration/4)
		})
	})
}

// mediaData writes an mdat of n bytes and returns where its payload starts.
func mediaData(t *testing.T, w *mp4.Writer, n int) uint32 {
	t.Helper()
	var at int64
	box(t, w, mp4.BoxTypeMdat(), func() {
		pos, err := w.Seek(0, io.SeekCurrent)
		if err != nil {
			t.Fatal(err)
		}
		at = pos
		if _, err := w.Write(make([]byte, n)); err != nil {
			t.Fatal(err)
		}
	})
	return uint32(at)
}

// sampleTable writes a minf whose stbl puts perChunk samples of size bytes, each delta ticks long, in
// each chunk at offsets.
func sampleTable(t *testing.T, w *mp4.Writer, offsets []uint32, perChunk, size, delta uint32) {
	t.Helper()
	count := perChunk * uint32(len(offsets))
	box(t, w, mp4.BoxTypeMinf(), func() {
		box(t, w, mp4.BoxTypeStbl(), func() {
			leaf(t, w, &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: count, SampleDelta: delta}}})
			leaf(t, w, &mp4.Stsz{SampleSize: size, SampleCount: count})
			leaf(t, w, &mp4.Stsc{EntryCount: 1, Entries: []mp4.StscEntry{{FirstChunk: 1, SamplesPerChunk: perChunk, SampleDescriptionIndex: 1}}})
			leaf(t, w, &mp4.Stco{EntryCount: uint32(len(offsets)), ChunkOffset: offsets})
		})
	})
}

func videoTrak(t *testing.T, w *mp4.Writer, id, timescale, duration uint32) {
	t.Helper()
	box(t, w, mp4.BoxTypeTrak(), func() {
		leaf(t, w, &mp4.Tkhd{TrackID: id})
		box(t, w, mp4.BoxTypeMdia(), func() {
			leaf(t, w, &mp4.Mdhd{Timescale: timescale, DurationV0: duration})
			leaf(t, w, &mp4.Hdlr{HandlerType: [4]byte{'v', 'i', 'd', 'e'}})
		})
	})
}
