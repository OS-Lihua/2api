package controller

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// probeAudioSeconds accepts real PCM or compressed audio framing. It never
// derives usage from the requested number of clips or their declared length.
func probeAudioSeconds(ctx context.Context, data []byte, mimeType string) (int64, bool) {
	if ctx.Err() != nil || len(data) == 0 {
		return 0, false
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		return probeWaveDuration(data)
	}
	ext := ""
	switch {
	case bytes.HasPrefix(data, []byte("ID3")), len(data) > 1 && data[0] == 0xff && data[1]&0xe0 == 0xe0:
		ext = ".mp3"
	case bytes.HasPrefix(data, []byte("fLaC")):
		ext = ".flac"
	case strings.HasPrefix(mimeType, "audio/aac"):
		ext = ".aac"
	default:
		return 0, false
	}
	duration, err := common.GetAudioDuration(ctx, bytes.NewReader(data), ext)
	if err != nil || ctx.Err() != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > 3600 {
		return 0, false
	}
	return int64(duration), true
}

type probeMP4Box struct {
	kind    string
	payload []byte
}

// probeMP4Boxes checks container sizes before interpreting any media metadata.
func probeMP4Boxes(data []byte) ([]probeMP4Box, bool) {
	boxes := []probeMP4Box{}
	for len(data) > 0 {
		if len(data) < 8 {
			return nil, false
		}
		size := uint64(binary.BigEndian.Uint32(data[:4]))
		header := uint64(8)
		if size == 1 {
			if len(data) < 16 {
				return nil, false
			}
			size = binary.BigEndian.Uint64(data[8:16])
			header = 16
		}
		if size == 0 {
			size = uint64(len(data))
		}
		if size < header || size > uint64(len(data)) {
			return nil, false
		}
		boxes = append(boxes, probeMP4Box{kind: string(data[4:8]), payload: data[header:size]})
		data = data[size:]
	}
	return boxes, true
}

// probeVideoSeconds requires video framing and a real media payload, rather
// than trusting a task status or a response's MIME type. Durations are bounded
// observations for probe comparison and never become accounting usage.
func probeVideoSeconds(data []byte) (int64, bool) {
	boxes, valid := probeMP4Boxes(data)
	if !valid {
		return 0, false
	}
	hasType, hasMedia, hasVideo := false, false, false
	var movie []byte
	for _, box := range boxes {
		switch box.kind {
		case "ftyp":
			hasType = len(box.payload) >= 8
		case "mdat":
			hasMedia = len(box.payload) > 0
		case "moov":
			movie = box.payload
		}
	}
	if !hasType || !hasMedia || len(movie) == 0 {
		return 0, false
	}
	children, valid := probeMP4Boxes(movie)
	if !valid {
		return 0, false
	}
	var scale, duration uint64
	for _, box := range children {
		if box.kind == "mvhd" && len(box.payload) >= 20 {
			switch box.payload[0] {
			case 0:
				scale = uint64(binary.BigEndian.Uint32(box.payload[12:16]))
				duration = uint64(binary.BigEndian.Uint32(box.payload[16:20]))
			case 1:
				if len(box.payload) >= 32 {
					scale = uint64(binary.BigEndian.Uint32(box.payload[20:24]))
					duration = binary.BigEndian.Uint64(box.payload[24:32])
				}
			}
		}
		if box.kind != "trak" {
			continue
		}
		track, ok := probeMP4Boxes(box.payload)
		if !ok {
			return 0, false
		}
		for _, item := range track {
			if item.kind != "mdia" {
				continue
			}
			media, ok := probeMP4Boxes(item.payload)
			if !ok {
				return 0, false
			}
			for _, handler := range media {
				if handler.kind == "hdlr" && len(handler.payload) >= 12 && string(handler.payload[8:12]) == "vide" {
					hasVideo = true
				}
			}
		}
	}
	if !hasVideo || scale == 0 || duration == 0 || duration > 60*scale {
		return 0, false
	}
	return int64(duration / scale), true
}
