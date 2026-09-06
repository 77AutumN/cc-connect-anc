package claudecode

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"

	"github.com/chenhg5/cc-connect/core"
	"golang.org/x/image/webp"
)

// The standard WebP decoder handles still frames. Extract just the first ANMF
// container and decode it with that library; no animation player or extra codec.
// Layout: https://developers.google.com/speed/webp/docs/riff_container
func firstWebPFrame(data []byte) ([]byte, bool, error) {
	if len(data) < 30 || string(data[12:16]) != "VP8X" || data[20]&2 == 0 {
		return nil, false, nil
	}
	bad := func() ([]byte, bool, error) { return nil, true, &core.ImageInputError{Key: core.MsgImageInvalid} }
	if int(binary.LittleEndian.Uint32(data[4:8]))+8 != len(data) || binary.LittleEndian.Uint32(data[16:20]) != 10 {
		return bad()
	}
	u24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	w, h := u24(data[24:27])+1, u24(data[27:30])+1
	if w > 8000 || h > 8000 {
		return nil, true, &core.ImageInputError{Key: core.MsgImageDimensions}
	}
	background := color.NRGBA{}
	animationSeen := false
	for offset := 30; offset < len(data); {
		if len(data)-offset < 8 {
			return bad()
		}
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if size > len(data)-offset-8 {
			return bad()
		}
		chunk := data[offset+8 : offset+8+size]
		switch string(data[offset : offset+4]) {
		case "ANIM":
			if size != 6 || animationSeen {
				return bad()
			}
			background = color.NRGBA{R: chunk[2], G: chunk[1], B: chunk[0], A: chunk[3]}
			animationSeen = true
		case "ANMF":
			if size < 24 || !animationSeen {
				return bad()
			}
			encoded, err := decodeFirstWebPFrame(chunk, w, h, background)
			return encoded, true, err
		}
		offset += 8 + size + (size & 1)
		if offset > len(data) {
			return bad()
		}
	}
	return bad()
}

func decodeFirstWebPFrame(chunk []byte, w, h int, background color.NRGBA) ([]byte, error) {
	u24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	x, y, fw, fh := u24(chunk[:3])*2, u24(chunk[3:6])*2, u24(chunk[6:9])+1, u24(chunk[9:12])+1
	if x+fw > w || y+fh > h {
		return nil, &core.ImageInputError{Key: core.MsgImageInvalid}
	}
	// The first-frame bitstream keeps its original resolution and alpha.
	frame := make([]byte, 30+len(chunk)-16)
	copy(frame, "RIFF")
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(frame)-8))
	copy(frame[8:16], "WEBPVP8X")
	binary.LittleEndian.PutUint32(frame[16:20], 10)
	if string(chunk[16:20]) == "ALPH" {
		frame[20] = 0x10
	}
	copy(frame[24:30], chunk[6:12])
	copy(frame[30:], chunk[16:])
	cfg, err := webp.DecodeConfig(bytes.NewReader(frame))
	if err != nil || cfg.Width != fw || cfg.Height != fh {
		return nil, &core.ImageInputError{Key: core.MsgImageInvalid}
	}
	decoded, err := webp.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil, &core.ImageInputError{Key: core.MsgImageInvalid}
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
	blend := draw.Over
	if chunk[15]&2 != 0 {
		blend = draw.Src
	}
	draw.Draw(canvas, image.Rect(x, y, x+fw, y+fh), decoded, decoded.Bounds().Min, blend)
	encoded, err := encodeFirstImageFrame(canvas)
	return encoded, err
}
