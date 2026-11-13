// Package videoduration reads how long a rendered video lasts from its MP4 header (fork, TXE-7329).
//
// It is the MP4 half of the go-mp4 duration reader TXE-6774 wrote for Tokun
// (tokun/internal/mediabound/video.go in txehq/txe), carried over with its hardening as it is: the
// boxes are visited by their headers, only moov's track boxes and each moof's fragment boxes are
// decoded, every sample's data must lie in an mdat of the file, and the read is bounded by
// ReadLimits. The WebM half and Tokun's metering rules are left out: the video settler reads the
// output of a vendor's video job, which every vendor in videoProviders delivers as MP4.
package videoduration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"

	mp4 "github.com/abema/go-mp4"
)

// ContainerMP4 is the one container Read reads.
const ContainerMP4 = "mp4"

// ErrUnmeasurable is a file whose header is truncated, inconsistent, beyond the read limits, not an
// MP4, or names no duration. A read error of the caller's reader is never this error.
var ErrUnmeasurable = errors.New("video duration is unmeasurable")

// Duration is what Read read of a video file's header. Each duration is rounded up to a whole
// millisecond, so it is never short.
type Duration struct {
	// Container is ContainerMP4.
	Container string
	// VideoMS is the longest video track's duration; 0 when the file has no video track.
	VideoMS int64
	// AudioMS is the longest audio track's duration; 0 when the file has no audio track.
	AudioMS int64
}

// LongestMS is the longer of the video and audio tracks.
func (d Duration) LongestMS() int64 { return max(d.VideoMS, d.AudioMS) }

// ReadLimits bound what Read reads of an untrusted file's header.
type ReadLimits struct {
	// MaxHeaderBytes is the most header bytes decoded: MP4 box payloads.
	MaxHeaderBytes int64
	// MaxBoxBytes is the largest single MP4 box payload decoded.
	MaxBoxBytes int64
	// MaxBoxes is the most MP4 boxes visited.
	MaxBoxes int
}

// DefaultReadLimits hold a two-hour fragmented MP4 at 60 frames per second with its audio (about
// 800,000 samples of 8 header bytes each) with room to spare.
var DefaultReadLimits = ReadLimits{MaxHeaderBytes: 32 << 20, MaxBoxBytes: 4 << 20, MaxBoxes: 1 << 18}

// errVideoReadLimit means the header needs more than the read limits allow; the input is unmeasurable.
var errVideoReadLimit = errors.New("video header exceeds the read limits")

// errVideoInvalid means the header is truncated, inconsistent or names no duration.
var errVideoInvalid = errors.New("video header is truncated or names no duration")

// Read reads the duration of the MP4 of size bytes behind r, with DefaultReadLimits. See
// ReadWithLimits.
func Read(ctx context.Context, r io.ReaderAt, size int64) (Duration, error) {
	return ReadWithLimits(ctx, r, size, DefaultReadLimits)
}

// ReadWithLimits reads the duration of the MP4 of size bytes behind r. It reads only the header: the
// boxes are visited by their headers and only moov's track boxes (tkhd, mdhd, hdlr, trex) and each
// moof's fragment boxes (tfhd, tfdt, trun) are decoded, so mdat is never read. A file that is
// truncated, inconsistent, beyond the limits, not an MP4 or names no duration is ErrUnmeasurable. A
// read error of r other than io.EOF, and the context's error, are returned as they are.
func ReadWithLimits(ctx context.Context, r io.ReaderAt, size int64, limits ReadLimits) (Duration, error) {
	src := &videoSource{ctx: ctx, r: r}
	d, err := readVideoDuration(src, size, limits)
	switch {
	case src.err != nil:
		return Duration{}, src.err
	case err != nil:
		return Duration{}, fmt.Errorf("%w: %w", ErrUnmeasurable, err)
	}
	return d, nil
}

func readVideoDuration(src *videoSource, size int64, limits ReadLimits) (Duration, error) {
	if size <= 0 {
		return Duration{}, fmt.Errorf("%w: the file is empty", errVideoInvalid)
	}
	return readMP4Duration(src, io.NewSectionReader(src, 0, size), size, limits)
}

// videoSource is r with the caller's context checked before every read, and the first read error that
// is not io.EOF kept: that error is the caller's, never the input's.
type videoSource struct {
	ctx context.Context
	r   io.ReaderAt
	err error
}

func (s *videoSource) ReadAt(p []byte, off int64) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if err := s.ctx.Err(); err != nil {
		s.err = err
		return 0, err
	}
	n, err := s.r.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}

// mp4Track is one moov/trak: its id, kind, timescale, the ticks its media reaches and what its edit
// list presents.
type mp4Track struct {
	id        uint32
	handler   string
	timescale uint32
	// ticks is the larger of mdhd's duration and the end of the last fragment run.
	ticks uint64
	// edits is the sum of the edit list's segment durations, in the movie's timescale: the presented
	// duration, which a repeated span or an empty edit makes longer than the media's. nil without one.
	edits  *big.Int
	hasID  bool
	hasHdr bool
	// stsz, stsc and chunks are moov's sample table: each sample's size, how many samples each chunk
	// holds, and where each chunk starts (stco or co64).
	stsz   *mp4.Stsz
	stsc   *mp4.Stsc
	chunks []uint64
	// stts is moov's timing table, and ctts its composition offsets: the media lasts at least until
	// the last sample's decode end plus the largest offset, whatever mdhd says.
	stts *mp4.Stts
	ctts *mp4.Ctts
	// hasChunks reports that the trak carried an stco or a co64.
	hasChunks bool
	// samples counts the track's samples, in moov and in every fragment; a track without one presents
	// no media and adds no duration.
	samples uint64
}

// mp4Fragment is the state of one moof/traf while its boxes are visited.
type mp4Fragment struct {
	header *mp4.Tfhd
	track  *mp4Track
	// cursor is the decode time, in the track's timescale, the next sample starts at, and presented the
	// latest a run's samples present until with their composition offsets.
	cursor, presented *big.Int
	// base is the offset a run's data offset counts from, and next the byte the next run's data starts
	// at when it states no data offset.
	base, next uint64
}

// mp4Extent is a span of sample data [start, end) a track's tables reference.
type mp4Extent struct{ start, end uint64 }

// mp4BoundedTrun decodes no more trun entries than payload bytes hold at the entry size its flags
// give, and none when its entries carry no field, so a crafted sample count allocates nothing beyond
// the box's payload.
type mp4BoundedTrun struct {
	mp4.Trun
	payload uint
}

func (t *mp4BoundedTrun) GetFieldLength(name string, ctx mp4.Context) uint {
	each := trunEntryBytes(t.GetFlags())
	if each == 0 {
		return 0
	}
	return min(t.Trun.GetFieldLength(name, ctx), t.payload/each)
}

// trunEntryBytes is the size of one trun entry under flags: four bytes per optional field present.
func trunEntryBytes(flags uint32) uint {
	var n uint
	for _, f := range []uint32{0x000100, 0x000200, 0x000400, 0x000800} {
		if flags&f != 0 {
			n += 4
		}
	}
	return n
}

func readMP4Duration(src *videoSource, rs io.ReadSeeker, size int64, limits ReadLimits) (Duration, error) {
	var (
		tracks   []*mp4Track
		byID     = map[uint32]*mp4Track{}
		trex     = map[uint32]*mp4.Trex{}
		current  *mp4Track
		fragment *mp4Fragment
		moov     bool
		// movieTimescale is mvhd's: the unit of every edit list's segment durations.
		movieTimescale uint32
		boxes          int
		decoded        int64
		// mdats are the payloads of the file's mdat boxes, in file order; runs are the fragment runs'
		// sample data. Every sample's data lies in an mdat, or the file was cut short.
		mdats []mp4Extent
		runs  []mp4Extent
		// moofOffset is the current moof's offset, and trafEnd where the previous traf's data ended:
		// the base of a traf that names none (nil for the first traf of a moof).
		moofOffset uint64
		trafEnd    *uint64
	)
	// decode reads the payload of the box h is on, within the limits.
	decode := func(h *mp4.ReadHandle) (mp4.IBox, error) {
		payload := int64(h.BoxInfo.Size - h.BoxInfo.HeaderSize)
		if payload > limits.MaxBoxBytes || payload > limits.MaxHeaderBytes-decoded {
			return nil, fmt.Errorf("%w: %s box of %d bytes", errVideoReadLimit, h.BoxInfo.Type, payload)
		}
		decoded += payload
		box, _, err := h.ReadPayload()
		return box, err
	}
	parent := func(h *mp4.ReadHandle, want string) error {
		if len(h.Path) < 2 || h.Path[len(h.Path)-2].String() != want {
			return fmt.Errorf("%w: %s outside %s", errVideoInvalid, h.BoxInfo.Type, want)
		}
		return nil
	}

	var handler mp4.ReadHandler
	handler = func(h *mp4.ReadHandle) (any, error) {
		bi := h.BoxInfo
		if boxes++; boxes > limits.MaxBoxes {
			return nil, fmt.Errorf("%w: more than %d boxes", errVideoReadLimit, limits.MaxBoxes)
		}
		// Every box, mdat included, ends within the file: a box past its end is a truncated file.
		if bi.Size < bi.HeaderSize || bi.Offset > uint64(size) || bi.Size > uint64(size)-bi.Offset {
			return nil, fmt.Errorf("%w: %s box at %d of %d bytes ends past the file's %d bytes", errVideoInvalid, bi.Type, bi.Offset, bi.Size, size)
		}
		if len(h.Path) > 8 {
			return nil, fmt.Errorf("%w: boxes nested %d deep", errVideoInvalid, len(h.Path))
		}
		switch typ := bi.Type.String(); typ {
		case "moov":
			if len(h.Path) != 1 || moov {
				return nil, fmt.Errorf("%w: a second or nested moov", errVideoInvalid)
			}
			moov = true
			_, err := h.Expand()
			return nil, err
		case "mdia":
			if err := parent(h, "trak"); err != nil {
				return nil, err
			}
			_, err := h.Expand()
			return nil, err
		case "mvex":
			if err := parent(h, "moov"); err != nil {
				return nil, err
			}
			_, err := h.Expand()
			return nil, err
		case "minf", "stbl":
			want := map[string]string{"minf": "mdia", "stbl": "minf"}[typ]
			if err := parent(h, want); err != nil {
				return nil, err
			}
			_, err := h.Expand()
			return nil, err
		case "stsz", "stsc", "stco", "co64", "stts", "ctts":
			if err := parent(h, "stbl"); err != nil {
				return nil, err
			}
			// Each entry has bytes, so go-mp4 stops at the payload's end whatever count is declared.
			box, err := decode(h)
			if err != nil {
				return nil, err
			}
			switch b := box.(type) {
			case *mp4.Stsz:
				if current.stsz != nil {
					return nil, fmt.Errorf("%w: a second stsz in one trak", errVideoInvalid)
				}
				if b.SampleSize == 0 && uint64(len(b.EntrySize)) != uint64(b.SampleCount) {
					return nil, fmt.Errorf("%w: an stsz of %d samples holds %d", errVideoInvalid, b.SampleCount, len(b.EntrySize))
				}
				current.stsz = b
			case *mp4.Stsc:
				if current.stsc != nil {
					return nil, fmt.Errorf("%w: a second stsc in one trak", errVideoInvalid)
				}
				if uint64(len(b.Entries)) != uint64(b.EntryCount) {
					return nil, fmt.Errorf("%w: an stsc of %d entries holds %d", errVideoInvalid, b.EntryCount, len(b.Entries))
				}
				current.stsc = b
			case *mp4.Stco:
				if current.hasChunks || uint64(len(b.ChunkOffset)) != uint64(b.EntryCount) {
					return nil, fmt.Errorf("%w: a second chunk table, or an stco holding fewer offsets than it declares", errVideoInvalid)
				}
				current.hasChunks = true
				for _, off := range b.ChunkOffset {
					current.chunks = append(current.chunks, uint64(off))
				}
			case *mp4.Co64:
				if current.hasChunks || uint64(len(b.ChunkOffset)) != uint64(b.EntryCount) {
					return nil, fmt.Errorf("%w: a second chunk table, or a co64 holding fewer offsets than it declares", errVideoInvalid)
				}
				current.hasChunks, current.chunks = true, b.ChunkOffset
			case *mp4.Stts:
				if current.stts != nil || uint64(len(b.Entries)) != uint64(b.EntryCount) {
					return nil, fmt.Errorf("%w: a second stts, or an stts holding fewer entries than it declares", errVideoInvalid)
				}
				current.stts = b
			case *mp4.Ctts:
				if current.ctts != nil || uint64(len(b.Entries)) != uint64(b.EntryCount) {
					return nil, fmt.Errorf("%w: a second ctts, or a ctts holding fewer entries than it declares", errVideoInvalid)
				}
				current.ctts = b
			}
			return nil, nil
		case "mdat":
			if len(h.Path) == 1 {
				mdats = append(mdats, mp4Extent{start: bi.Offset + bi.HeaderSize, end: bi.Offset + bi.Size})
			}
			return nil, nil
		case "mvhd":
			if err := parent(h, "moov"); err != nil {
				return nil, err
			}
			box, err := decode(h)
			if err != nil {
				return nil, err
			}
			movieTimescale = box.(*mp4.Mvhd).Timescale
			return nil, nil
		case "edts":
			if err := parent(h, "trak"); err != nil {
				return nil, err
			}
			_, err := h.Expand()
			return nil, err
		case "elst":
			if err := parent(h, "edts"); err != nil {
				return nil, err
			}
			if current.edits != nil {
				return nil, fmt.Errorf("%w: a second elst in one trak", errVideoInvalid)
			}
			// Each entry has bytes, so go-mp4 stops at the payload's end whatever count is declared.
			box, err := decode(h)
			if err != nil {
				return nil, err
			}
			elst := box.(*mp4.Elst)
			if uint64(len(elst.Entries)) != uint64(elst.EntryCount) {
				return nil, fmt.Errorf("%w: an elst of %d entries holds %d", errVideoInvalid, elst.EntryCount, len(elst.Entries))
			}
			current.edits = new(big.Int)
			for i := range elst.Entries {
				current.edits.Add(current.edits, new(big.Int).SetUint64(elst.GetSegmentDuration(i)))
			}
			return nil, nil
		case "trak":
			if err := parent(h, "moov"); err != nil {
				return nil, err
			}
			current = &mp4Track{}
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			if !current.hasID || !current.hasHdr || current.timescale == 0 {
				return nil, fmt.Errorf("%w: a trak without its tkhd, mdhd or hdlr", errVideoInvalid)
			}
			if _, dup := byID[current.id]; dup {
				return nil, fmt.Errorf("%w: two traks with track id %d", errVideoInvalid, current.id)
			}
			if err := reconcileTiming(current); err != nil {
				return nil, err
			}
			tracks, byID[current.id], current = append(tracks, current), current, nil
			return nil, nil
		case "tkhd":
			if err := parent(h, "trak"); err != nil {
				return nil, err
			}
			box, err := decode(h)
			if err != nil {
				return nil, err
			}
			current.id, current.hasID = box.(*mp4.Tkhd).TrackID, true
			return nil, nil
		case "mdhd":
			if err := parent(h, "mdia"); err != nil {
				return nil, err
			}
			box, err := decode(h)
			if err != nil {
				return nil, err
			}
			mdhd := box.(*mp4.Mdhd)
			current.timescale, current.ticks = mdhd.Timescale, mdhd.GetDuration()
			return nil, nil
		case "hdlr":
			if len(h.Path) < 2 || h.Path[len(h.Path)-2].String() != "mdia" {
				// minf's and meta's hdlr name a data or metadata handler, not the track's kind.
				return nil, nil
			}
			box, err := decode(h)
			if err != nil {
				return nil, err
			}
			current.handler, current.hasHdr = string(box.(*mp4.Hdlr).HandlerType[:]), true
			return nil, nil
		case "trex":
			if err := parent(h, "mvex"); err != nil {
				return nil, err
			}
			box, err := decode(h)
			if err != nil {
				return nil, err
			}
			t := box.(*mp4.Trex)
			trex[t.TrackID] = t
			return nil, nil
		case "moof":
			if len(h.Path) != 1 || !moov {
				return nil, fmt.Errorf("%w: a moof before moov or nested", errVideoInvalid)
			}
			moofOffset, trafEnd = bi.Offset, nil
			_, err := h.Expand()
			return nil, err
		case "traf":
			if err := parent(h, "moof"); err != nil {
				return nil, err
			}
			fragment = &mp4Fragment{}
			if _, err := h.Expand(); err != nil {
				return nil, err
			}
			if fragment.track == nil {
				return nil, fmt.Errorf("%w: a traf without its tfhd", errVideoInvalid)
			}
			if !fragment.cursor.IsUint64() || !fragment.presented.IsUint64() {
				return nil, fmt.Errorf("%w: a fragment ends past 2^64 ticks", errVideoInvalid)
			}
			fragment.track.ticks = max(fragment.track.ticks, fragment.cursor.Uint64(), fragment.presented.Uint64())
			end := fragment.next
			trafEnd, fragment = &end, nil
			return nil, nil
		case "tfhd":
			if err := parent(h, "traf"); err != nil {
				return nil, err
			}
			box, err := decode(h)
			if err != nil {
				return nil, err
			}
			tfhd := box.(*mp4.Tfhd)
			track, ok := byID[tfhd.TrackID]
			if !ok || fragment.header != nil {
				return nil, fmt.Errorf("%w: a tfhd for track %d, which moov does not hold, or a second tfhd", errVideoInvalid, tfhd.TrackID)
			}
			// Without a tfdt, a fragment continues where the track's media ends.
			fragment.header, fragment.track, fragment.cursor = tfhd, track, new(big.Int).SetUint64(track.ticks)
			fragment.presented = new(big.Int).Set(fragment.cursor)
			// A run's data offset counts from the tfhd's base, else the moof, which is also where a first
			// traf's data starts; a later traf without either starts where the previous one's ended.
			switch {
			case tfhd.CheckFlag(mp4.TfhdBaseDataOffsetPresent):
				fragment.base = tfhd.BaseDataOffset
			case tfhd.CheckFlag(mp4.TfhdDefaultBaseIsMoof) || trafEnd == nil:
				fragment.base = moofOffset
			default:
				fragment.base = *trafEnd
			}
			fragment.next = fragment.base
			return nil, nil
		case "tfdt":
			if err := parent(h, "traf"); err != nil {
				return nil, err
			}
			if fragment.header == nil {
				return nil, fmt.Errorf("%w: a tfdt before its tfhd", errVideoInvalid)
			}
			box, err := decode(h)
			if err != nil {
				return nil, err
			}
			fragment.cursor.SetUint64(box.(*mp4.Tfdt).GetBaseMediaDecodeTime())
			return nil, nil
		case "trun":
			if err := parent(h, "traf"); err != nil {
				return nil, err
			}
			if fragment.header == nil {
				return nil, fmt.Errorf("%w: a trun before its tfhd", errVideoInvalid)
			}
			payload := int64(bi.Size - bi.HeaderSize)
			if payload > limits.MaxBoxBytes || payload > limits.MaxHeaderBytes-decoded {
				return nil, fmt.Errorf("%w: trun box of %d bytes", errVideoReadLimit, payload)
			}
			decoded += payload
			run, err := readTrun(io.NewSectionReader(src, int64(bi.Offset+bi.HeaderSize), payload), bi.Context,
				fragment.header, trex[fragment.header.TrackID])
			if err != nil {
				return nil, err
			}
			start := new(big.Int).SetUint64(fragment.next)
			if run.dataOffset != nil {
				start.SetUint64(fragment.base).Add(start, big.NewInt(*run.dataOffset))
			}
			end := new(big.Int).Add(start, run.bytes)
			if start.Sign() < 0 || !end.IsUint64() {
				return nil, fmt.Errorf("%w: a fragment run's data lies outside the file", errVideoInvalid)
			}
			if run.bytes.Sign() > 0 {
				runs = append(runs, mp4Extent{start: start.Uint64(), end: end.Uint64()})
			}
			fragment.next = end.Uint64()
			fragment.track.samples += uint64(run.count)
			fragment.cursor.Add(fragment.cursor, run.ticks)
			if presented := new(big.Int).Add(fragment.cursor, big.NewInt(run.offset)); presented.Cmp(fragment.presented) > 0 {
				fragment.presented = presented
			}
			return nil, nil
		}
		return nil, nil
	}
	if _, err := mp4.ReadBoxStructure(rs, handler); err != nil {
		return Duration{}, fmt.Errorf("%w: %w", errVideoInvalid, err)
	}
	if src.err != nil {
		return Duration{}, src.err
	}
	if !moov {
		return Duration{}, fmt.Errorf("%w: no moov box", errVideoInvalid)
	}
	// Every sample's data lies in an mdat of the file: data a table points to that is not there is a
	// file cut short before or between its mdats, whose header still reads whole.
	inMdat := func(e mp4Extent) bool {
		i := sort.Search(len(mdats), func(i int) bool { return mdats[i].start > e.start }) - 1
		return i >= 0 && e.end <= mdats[i].end
	}
	for _, e := range runs {
		if !inMdat(e) {
			return Duration{}, fmt.Errorf("%w: a fragment run's data at [%d, %d) is in no mdat of the file", errVideoInvalid, e.start, e.end)
		}
	}
	for _, t := range tracks {
		if err := checkChunks(t, inMdat); err != nil {
			return Duration{}, err
		}
	}
	d := Duration{Container: ContainerMP4}
	for _, t := range tracks {
		if t.samples == 0 {
			continue
		}
		ms, err := ticksToMS(new(big.Int).SetUint64(t.ticks), t.timescale)
		if err != nil {
			return Duration{}, err
		}
		// The hold takes the longer of the media and what the edit list presents, so it is never short.
		if t.edits != nil {
			if movieTimescale == 0 {
				return Duration{}, fmt.Errorf("%w: an edit list with no movie timescale", errVideoInvalid)
			}
			edited, err := ticksToMS(t.edits, movieTimescale)
			if err != nil {
				return Duration{}, err
			}
			ms = max(ms, edited)
		}
		switch t.handler {
		case "vide":
			d.VideoMS = max(d.VideoMS, ms)
		case "soun":
			d.AudioMS = max(d.AudioMS, ms)
		}
	}
	if d.LongestMS() == 0 {
		return Duration{}, fmt.Errorf("%w: no video or audio track with a duration", errVideoInvalid)
	}
	return d, nil
}

// reconcileTiming raises t.ticks, mdhd's duration, to what the timing table presents: the sum of stts's
// sample deltas plus the largest positive composition offset in ctts. stts must time exactly the
// samples stsz sizes, or the table is inconsistent.
func reconcileTiming(t *mp4Track) error {
	if t.stts == nil {
		return nil
	}
	end, timed := new(big.Int), new(big.Int)
	for _, e := range t.stts.Entries {
		n := big.NewInt(int64(e.SampleCount))
		timed.Add(timed, n)
		end.Add(end, n.Mul(n, big.NewInt(int64(e.SampleDelta))))
	}
	if t.stsz == nil || timed.Cmp(new(big.Int).SetUint64(uint64(t.stsz.SampleCount))) != 0 {
		return fmt.Errorf("%w: stts times %s samples where stsz sizes another count", errVideoInvalid, timed)
	}
	if t.ctts != nil {
		var offset int64
		for i := range t.ctts.Entries {
			offset = max(offset, t.ctts.GetSampleOffset(i))
		}
		end.Add(end, big.NewInt(offset))
	}
	if !end.IsUint64() {
		return fmt.Errorf("%w: the timing table runs past 2^64 ticks", errVideoInvalid)
	}
	t.ticks = max(t.ticks, end.Uint64())
	return nil
}

// checkChunks walks a track's moov sample table, adds its samples to t.samples, and checks that each
// chunk's data, from its offset over the sizes of the samples stsc puts in it, lies where inMdat says
// there is media. A trak with no sample table holds no samples in moov.
func checkChunks(t *mp4Track, inMdat func(mp4Extent) bool) error {
	if t.stsz == nil && t.stsc == nil && !t.hasChunks && t.stts == nil {
		return nil
	}
	if t.stsz == nil || t.stsc == nil || !t.hasChunks || t.stts == nil {
		return fmt.Errorf("%w: a sample table without its stsz, stsc, stts or chunk offsets", errVideoInvalid)
	}
	count := uint64(t.stsz.SampleCount)
	var used uint64
	runs := t.stsc.Entries
	r := -1
	for i, off := range t.chunks {
		chunk := uint64(i) + 1
		for r+1 < len(runs) && uint64(runs[r+1].FirstChunk) <= chunk {
			if r >= 0 && runs[r+1].FirstChunk <= runs[r].FirstChunk {
				return fmt.Errorf("%w: stsc's first chunks do not increase", errVideoInvalid)
			}
			r++
		}
		if r < 0 {
			return fmt.Errorf("%w: chunk %d has no stsc entry", errVideoInvalid, chunk)
		}
		n := uint64(runs[r].SamplesPerChunk)
		if n > count-used {
			return fmt.Errorf("%w: chunk %d holds samples past stsz's %d", errVideoInvalid, chunk, count)
		}
		size := new(big.Int)
		if t.stsz.SampleSize != 0 {
			size.Mul(new(big.Int).SetUint64(n), big.NewInt(int64(t.stsz.SampleSize)))
		} else {
			for _, s := range t.stsz.EntrySize[used : used+n] {
				size.Add(size, big.NewInt(int64(s)))
			}
		}
		used += n
		if size.Sign() == 0 {
			continue
		}
		end := size.Add(size, new(big.Int).SetUint64(off))
		if !end.IsUint64() || !inMdat(mp4Extent{start: off, end: end.Uint64()}) {
			return fmt.Errorf("%w: chunk %d's data from %d is in no mdat of the file", errVideoInvalid, chunk, off)
		}
	}
	if used != count {
		return fmt.Errorf("%w: the chunks hold %d of stsz's %d samples", errVideoInvalid, used, count)
	}
	t.samples += count
	return nil
}

// mp4Run is what one trun holds: how long its samples last, how many bytes they take, how many there
// are, and its data offset when it states one.
type mp4Run struct {
	ticks, bytes *big.Int
	count        uint32
	dataOffset   *int64
	// offset is the largest positive composition offset of the run's samples: the last one presents
	// that much after its decode end at most.
	offset int64
}

// readTrun reads one trun's payload: each sample's duration and size are its own, or else the tfhd's
// default, or else the track's trex default.
func readTrun(payload *io.SectionReader, ctx mp4.Context, tfhd *mp4.Tfhd, trex *mp4.Trex) (mp4Run, error) {
	trun := mp4BoundedTrun{payload: uint(payload.Size())}
	if _, err := mp4.Unmarshal(payload, uint64(payload.Size()), &trun, ctx); err != nil {
		return mp4Run{}, fmt.Errorf("%w: trun: %w", errVideoInvalid, err)
	}
	flags := trun.GetFlags()
	if trunEntryBytes(flags) > 0 && uint64(len(trun.Entries)) != uint64(trun.SampleCount) {
		return mp4Run{}, fmt.Errorf("%w: a trun of %d samples holds %d", errVideoInvalid, trun.SampleCount, len(trun.Entries))
	}
	if trun.SampleCount != 0 && tfhd.CheckFlag(mp4.TfhdDurationIsEmpty) {
		return mp4Run{}, fmt.Errorf("%w: a trun with samples in a fragment marked empty", errVideoInvalid)
	}
	run := mp4Run{count: trun.SampleCount}
	if flags&0x000001 != 0 {
		offset := int64(trun.DataOffset)
		run.dataOffset = &offset
	}
	if flags&0x000800 != 0 {
		for _, e := range trun.Entries {
			offset := int64(e.SampleCompositionTimeOffsetV0)
			if trun.GetVersion() != 0 {
				offset = int64(e.SampleCompositionTimeOffsetV1)
			}
			run.offset = max(run.offset, offset)
		}
	}
	var err error
	run.ticks, err = trunSum(trun, 0x000100, func(e mp4.TrunEntry) uint32 { return e.SampleDuration },
		tfhd.CheckFlag(mp4.TfhdDefaultSampleDurationPresent), tfhd.DefaultSampleDuration, trex,
		func(t *mp4.Trex) uint32 { return t.DefaultSampleDuration })
	if err != nil {
		return mp4Run{}, fmt.Errorf("%w: a trun with no sample duration and no default", err)
	}
	run.bytes, err = trunSum(trun, 0x000200, func(e mp4.TrunEntry) uint32 { return e.SampleSize },
		tfhd.CheckFlag(mp4.TfhdDefaultSampleSizePresent), tfhd.DefaultSampleSize, trex,
		func(t *mp4.Trex) uint32 { return t.DefaultSampleSize })
	if err != nil {
		return mp4Run{}, fmt.Errorf("%w: a trun with no sample size and no default", err)
	}
	return run, nil
}

// trunSum is the sum of one per-sample field over a trun: each entry's own value when the trun carries
// the field (flag), else the sample count times the tfhd's default when it has one, else the trex's.
func trunSum(trun mp4BoundedTrun, flag uint32, own func(mp4.TrunEntry) uint32, hasTfhd bool, tfhdDefault uint32,
	trex *mp4.Trex, trexDefault func(*mp4.Trex) uint32) (*big.Int, error) {
	if trun.GetFlags()&flag != 0 {
		sum := new(big.Int)
		for _, entry := range trun.Entries {
			sum.Add(sum, big.NewInt(int64(own(entry))))
		}
		return sum, nil
	}
	var each uint32
	switch {
	case hasTfhd:
		each = tfhdDefault
	case trex != nil:
		each = trexDefault(trex)
	case trun.SampleCount == 0:
		return new(big.Int), nil
	default:
		return nil, errVideoInvalid
	}
	return new(big.Int).Mul(big.NewInt(int64(trun.SampleCount)), big.NewInt(int64(each))), nil
}

// ticksToMS is ceil(ticks × 1000 ÷ timescale), which must fit int64.
func ticksToMS(ticks *big.Int, timescale uint32) (int64, error) {
	q, rem := new(big.Int).QuoRem(new(big.Int).Mul(ticks, big.NewInt(1000)), big.NewInt(int64(timescale)), new(big.Int))
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("%w: a duration of %d ticks at %d per second", errVideoInvalid, ticks, timescale)
	}
	return q.Int64(), nil
}
