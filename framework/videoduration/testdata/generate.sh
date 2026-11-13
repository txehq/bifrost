#!/usr/bin/env bash
# Regenerates the video fixtures mp4_test.go reads (TXE-6774's, carried over by TXE-7329). Every file is 12 seconds of ffmpeg's lavfi
# testsrc at 64x48 and 10 frames per second; the ones with sound carry a 440 Hz sine track.
set -euo pipefail
cd "$(dirname "$0")"

video=(-f lavfi -i testsrc=size=64x48:rate=10:duration=12)
audio=(-f lavfi -i sine=frequency=440:sample_rate=16000:duration=12)
global=(-hide_banner -loglevel error -y)
out=(-map_metadata -1 -fflags +bitexact -flags:v +bitexact -flags:a +bitexact)

# moov after mdat (ffmpeg's default): the reader seeks past the media to reach it.
ffmpeg "${global[@]}" "${video[@]}" "${audio[@]}" "${out[@]}" -c:v mpeg4 -q:v 31 -c:a aac -b:a 16k twelve-seconds.mp4
# moov before mdat.
ffmpeg "${global[@]}" "${video[@]}" "${audio[@]}" "${out[@]}" -c:v mpeg4 -q:v 31 -c:a aac -b:a 16k \
	-movflags +faststart twelve-seconds-faststart.mp4
# no audio track.
ffmpeg "${global[@]}" "${video[@]}" "${out[@]}" -c:v mpeg4 -q:v 31 twelve-seconds-silent.mp4
# fragmented: an empty moov, then one moof and mdat per 2-second group of pictures.
ffmpeg "${global[@]}" "${video[@]}" "${audio[@]}" "${out[@]}" -c:v mpeg4 -q:v 31 -g 20 -c:a aac -b:a 16k \
	-movflags frag_keyframe+empty_moov+default_base_moof twelve-seconds-fragmented.mp4
