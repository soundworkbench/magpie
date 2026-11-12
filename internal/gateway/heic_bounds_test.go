package gateway

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"testing"

	"github.com/gen2brain/h265/heic"
	"github.com/yetone/magpie/internal/provider"
)

// smallHEVC uses the decoder's encoder so ordinary tests need no external tool.
func smallHEVC(t *testing.T) ([]byte, []byte) {
	t.Helper()
	var out bytes.Buffer
	if err := heic.Encode(&out, image.NewGray(image.Rect(0, 0, 64, 64)), heic.EncodeOptions{Lossless: true}); err != nil {
		t.Fatal(err)
	}
	raw := out.Bytes()
	property := func(typ string) []byte {
		i := bytes.Index(raw, []byte(typ))
		if i < 4 {
			t.Fatalf("encoder omitted %s", typ)
		}
		n := int(binary.BigEndian.Uint32(raw[i-4 : i]))
		if n < 8 || i-4+n > len(raw) {
			t.Fatalf("invalid %s", typ)
		}
		return raw[i+4 : i-4+n]
	}
	return property("hvcC"), property("mdat")
}

func TestHEICInconsistentSizeGoesAsItCame(t *testing.T) {
	cfg, sample := smallHEVC(t)
	for _, grid := range []bool{false, true} {
		name := "ispe"
		if grid {
			name = "grid"
		}
		t.Run(name, func(t *testing.T) {
			raw := probeHEIC(cfg, sample, 16, grid, false)
			body := probeBody(raw)
			if got := heicAsJPEG(provider.Responses, body); !bytes.Equal(got, body) {
				t.Fatal("an image whose coded size disagrees with its header was converted")
			}
		})
	}
}

func TestHEICSmallGeneratedStillConverts(t *testing.T) {
	cfg, sample := smallHEVC(t)
	body := probeBody(probeHEIC(cfg, sample, 64, false, false))
	if got := heicAsJPEG(provider.Responses, body); bytes.Equal(got, body) {
		t.Fatal("an honest small image was not converted")
	}
}

func TestHEICGridLayout(t *testing.T) {
	for _, depth := range []int{8, 10} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			var out bytes.Buffer
			if err := heic.Encode(&out, image.NewGray(image.Rect(0, 0, 96, 96)), heic.EncodeOptions{Lossless: true, Tile: 64, BitDepth: depth}); err != nil {
				t.Fatal(err)
			}
			raw := out.Bytes()
			if !heicWithinBudget(raw) {
				t.Fatal("an honest grid with cropped edges was refused")
			}
			body := probeBody(raw)
			if got := heicAsJPEG(provider.Responses, body); bytes.Equal(got, body) {
				t.Fatal("an honest grid did not convert")
			}
			// Keep every tile honest while shrinking only the output and its ispe.
			header := bytes.Index(raw, []byte("ispe"))
			if header < 0 {
				t.Fatal("no grid ispe")
			}
			broken := bytes.Clone(raw)
			binary.BigEndian.PutUint32(broken[header+8:], 32)
			binary.BigEndian.PutUint32(broken[header+12:], 32)
			// The encoder puts the grid descriptor first in mdat.
			media := bytes.Index(broken, []byte("mdat"))
			if media < 0 {
				t.Fatal("no grid data")
			}
			binary.BigEndian.PutUint32(broken[media+8:], 32)
			binary.BigEndian.PutUint32(broken[media+12:], 32)
			body = probeBody(broken)
			if got := heicAsJPEG(provider.Responses, body); !bytes.Equal(got, body) {
				t.Fatal("a grid whose extra row and column lie outside the output was converted")
			}
		})
	}
}

func TestHEICConfigCannotCarryPictures(t *testing.T) {
	cfg, sample := smallHEVC(t)
	n := int(binary.BigEndian.Uint32(sample))
	nal := sample[4 : 4+n]
	cfg = bytes.Clone(cfg)
	cfg[22]++
	cfg = append(cfg, nal[0]>>1&63, 0, 1)
	cfg = binary.BigEndian.AppendUint16(cfg, uint16(len(nal)))
	cfg = append(cfg, nal...)
	body := probeBody(probeHEIC(cfg, sample, 64, false, false))
	if got := heicAsJPEG(provider.Responses, body); !bytes.Equal(got, body) {
		t.Fatal("a picture hidden in decoder configuration was converted")
	}
}

func TestHEICDuplicateLocationsGoAsTheyCame(t *testing.T) {
	cfg, sample := smallHEVC(t)
	raw := probeHEIC(cfg, sample, 64, false, false)
	i := bytes.Index(raw, []byte("iloc"))
	n := int(binary.BigEndian.Uint32(raw[i-4:]))
	// A second entry with no extents still overwrites the decoder's base.
	extra := []byte{0, 1, 0, 0, 0, 0}
	pos := i - 4 + n
	changed := append(bytes.Clone(raw[:pos]), extra...)
	changed = append(changed, raw[pos:]...)
	binary.BigEndian.PutUint32(changed[i-4:], uint32(n+len(extra)))
	binary.BigEndian.PutUint16(changed[i+10:], 2)
	binary.BigEndian.PutUint32(changed[i+18:], binary.BigEndian.Uint32(changed[i+18:])+uint32(len(extra)))
	meta := bytes.Index(changed, []byte("meta"))
	binary.BigEndian.PutUint32(changed[meta-4:], binary.BigEndian.Uint32(changed[meta-4:])+uint32(len(extra)))
	body := probeBody(changed)
	if got := heicAsJPEG(provider.Responses, body); !bytes.Equal(got, body) {
		t.Fatal("duplicate locations were converted")
	}
}

func TestHEICRepeatedPropertiesGoAsTheyCame(t *testing.T) {
	cfg, sample := smallHEVC(t)
	props := [][]byte{probeBox("hvcC", cfg), probeISPE(64, 64)}
	assoc := append([]byte{1, 2}, bytes.Repeat([]byte{1}, 64)...)
	raw := probeItems([]string{"hvc1"}, [][]byte{sample}, props, [][]byte{assoc}, nil)
	body := probeBody(raw)
	if got := heicAsJPEG(provider.Responses, body); !bytes.Equal(got, body) {
		t.Fatal("repeated properties were converted")
	}
}

func TestHEICPhoneGridHeadersFit(t *testing.T) {
	for _, depth := range []int{8, 10} {
		var out bytes.Buffer
		if err := heic.Encode(&out, image.NewGray(image.Rect(0, 0, 512, 512)), heic.EncodeOptions{Quality: 1, BitDepth: depth}); err != nil {
			t.Fatal(err)
		}
		raw := out.Bytes()
		part := func(kind string) []byte {
			i := bytes.Index(raw, []byte(kind))
			if i < 4 {
				t.Fatalf("missing %s", kind)
			}
			n := int(binary.BigEndian.Uint32(raw[i-4:]))
			return raw[i+4 : i-4+n]
		}
		cfg, sample := part("hvcC"), part("mdat")
		for _, size := range [][2]int{{4032, 3024}, {6000, 4000}, {8000, 6000}} {
			grid := probePhoneGrid(cfg, sample, size[0], size[1], 512)
			if !heicWithinBudget(grid) {
				t.Fatalf("%dx%d depth %d honest tiled photo was refused", size[0], size[1], depth)
			}
		}
	}
}

func TestHEICExtraDecoderStateGoesAsItCame(t *testing.T) {
	cfg, sample := smallHEVC(t)
	good := probeHEIC(cfg, sample, 64, false, false)
	t.Run("track", func(t *testing.T) {
		raw := append(bytes.Clone(good), probeBox("moov")...)
		// parseMoov accepts an empty movie; the primary item still decodes.
		if _, err := heic.Decode(bytes.NewReader(raw)); err != nil {
			t.Fatalf("track control does not decode: %v", err)
		}
		body := probeBody(raw)
		if got := heicAsJPEG(provider.Responses, body); !bytes.Equal(got, body) {
			t.Fatal("an unchecked picture track was converted")
		}
	})
	t.Run("unused config", func(t *testing.T) {
		// Zero-length parameter sets still grow the upstream header's slice.
		extra := append(bytes.Clone(cfg[:23]), 0x80, 0, 65)
		extra[22] = 1
		extra = append(extra, make([]byte, 130)...)
		raw := probeItems([]string{"hvc1"}, [][]byte{sample}, [][]byte{probeBox("hvcC", cfg), probeISPE(64, 64), probeBox("hvcC", extra)}, [][]byte{{1, 2}}, nil)
		body := probeBody(raw)
		if got := heicAsJPEG(provider.Responses, body); !bytes.Equal(got, body) {
			t.Fatal("an unchecked unused decoder configuration was converted")
		}
	})
}
