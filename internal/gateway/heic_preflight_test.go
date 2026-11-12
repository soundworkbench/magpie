package gateway

import (
	"bytes"
	"encoding/binary"
	"os"
	"runtime"
	"testing"

	"github.com/gen2brain/h265/hevc"
	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

// prefixConfig describes the SPS fields the preflight reads, without pixels.
func prefixConfig(w, h, cropX, cropY, depth int) ([]byte, []byte) {
	var bits []byte
	put := func(v uint64, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, byte(v>>i&1))
		}
	}
	ue := func(v int) {
		n := 0
		for x := v + 1; x > 1; x >>= 1 {
			n++
		}
		put(0, n)
		put(uint64(v+1), n+1)
	}
	put(1, 8)
	put(0, 64)
	put(0, 32)
	ue(0)
	ue(1)
	ue(w)
	ue(h)
	if cropX != 0 || cropY != 0 {
		put(1, 1)
		ue(0)
		ue(cropX / 2)
		ue(0)
		ue(cropY / 2)
	} else {
		put(0, 1)
	}
	ue(depth - 8)
	ue(depth - 8)
	put(1, 1)
	rbsp := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		rbsp[i/8] |= b << uint(7-i%8)
	}
	nal := hevc.MarshalNAL(hevc.NALUnit{Type: hevc.NALSPS, RBSP: rbsp})
	cfg := make([]byte, 23)
	cfg[0] = 1
	cfg[16] = 1
	cfg[17] = byte(depth - 8)
	cfg[18] = byte(depth - 8)
	cfg[21] = 3
	cfg[22] = 1
	cfg = append(cfg, 33, 0, 1)
	cfg = binary.BigEndian.AppendUint16(cfg, uint16(len(nal)))
	cfg = append(cfg, nal...)
	return cfg, nal
}

// sharedGrid puts each tile's extents into the same small mdat. Offsets in
// spans are relative to that payload; no repeated media is stored in the file.
func sharedGrid(t *testing.T, cfg, media []byte, spans [][2]int, count int) []byte {
	t.Helper()
	raw := probePhoneGrid(cfg, []byte{0}, 64*count, 64, 64)
	boxes, ok := heicBoxes(raw)
	if !ok {
		t.Fatal("bad helper boxes")
	}
	var meta []heicBox
	for _, b := range boxes {
		if b.kind == "meta" {
			meta, ok = heicBoxes(b.data[4:])
		}
	}
	if !ok {
		t.Fatal("bad helper meta")
	}
	desc := []byte{0, 1, 0, byte(count - 1)}
	desc = binary.BigEndian.AppendUint32(desc, uint32(64*count))
	desc = binary.BigEndian.AppendUint32(desc, 64)
	build := func(base int) []byte {
		loc := binary.BigEndian.AppendUint16([]byte{0, 0, 0, 0, 0x44, 0}, uint16(count+1))
		for i := 0; i <= count; i++ {
			es := spans
			if i == 0 {
				es = [][2]int{{0, len(desc)}}
			}
			loc = binary.BigEndian.AppendUint16(loc, uint16(i+1))
			loc = append(loc, 0, 0)
			loc = binary.BigEndian.AppendUint16(loc, uint16(len(es)))
			for _, e := range es {
				off := e[0]
				if i != 0 {
					off += len(desc)
				}
				loc = binary.BigEndian.AppendUint32(loc, uint32(base+off))
				loc = binary.BigEndian.AppendUint32(loc, uint32(e[1]))
			}
		}
		parts := [][]byte{make([]byte, 4)}
		for _, b := range meta {
			if b.kind == "iloc" {
				parts = append(parts, probeBox("iloc", loc))
			} else {
				parts = append(parts, probeBox(b.kind, b.data))
			}
		}
		return probeBox("meta", parts...)
	}
	ftyp := probeBox("ftyp", []byte("heic\x00\x00\x00\x00heicmif1"))
	m := build(0)
	m = build(len(ftyp) + len(m) + 8)
	return bytes.Join([][]byte{ftyp, m, probeBox("mdat", desc, media)}, nil)
}

func TestHEICPreflightAllocationBound(t *testing.T) {
	cfg, sample := smallHEVC(t)
	for _, shape := range []string{"extents", "sps"} {
		t.Run(shape, func(t *testing.T) {
			var media []byte
			var spans [][2]int
			if shape == "extents" {
				media = bytes.Clone(sample)
				sei := append([]byte{78, 1}, bytes.Repeat([]byte{7}, 24<<10)...)
				media = binary.BigEndian.AppendUint32(media, uint32(len(sei)))
				media = append(media, sei...)
				spans = append(spans, [2]int{0, len(sample)})
				for range 15 {
					spans = append(spans, [2]int{len(sample), 4 + len(sei)})
				}
			} else {
				// Use the real SPS prefix so all shared in-band SPSs agree with hvcC.
				r := heicReader{data: cfg[23:]}
				var sps []byte
				for range int(cfg[22]) {
					r.num(1)
					n := r.num(2)
					for range n {
						nal := r.take(int(r.num(2)))
						if nal[0]>>1&63 == 33 {
							sps = bytes.Clone(nal)
						}
					}
				}
				sps = append(sps, bytes.Repeat([]byte{0, 0, 3}, 64<<10)...)
				media = binary.BigEndian.AppendUint32(nil, uint32(len(sps)))
				media = append(media, sps...)
				media = append(media, sample...)
				spans = [][2]int{{0, len(media)}}
			}
			raw := sharedGrid(t, cfg, media, spans, 32)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			accepted := heicWithinBudget(raw)
			runtime.ReadMemStats(&after)
			alloc := after.TotalAlloc - before.TotalAlloc
			bound := uint64(16*len(raw) + (1 << 20))
			t.Logf("input=%d TotalAlloc=%d bound=%d accepted=%t", len(raw), alloc, bound, accepted)
			if alloc > bound {
				t.Fatalf("preflight amplified input allocation: %d > %d", alloc, bound)
			}
			if accepted {
				t.Fatal("preflight accepted repeated payload amplification")
			}
		})
	}
}

func TestHEICLargeConformanceCropRefused(t *testing.T) {
	cfg, _ := prefixConfig(1024, 1024, 1016, 1016, 8)
	raw := probeHEIC(cfg, []byte{0, 0, 0, 3, 38, 1, 128}, 8, false, false)
	if heicWithinBudget(raw) {
		t.Fatal("large conformance crop hid the coded size")
	}
}

func TestHEICSmallConformanceCropConverts(t *testing.T) {
	raw, err := os.ReadFile("testdata/opaque.heic")
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(raw, []byte("hvcC"))
	if i < 4 {
		t.Fatal("fixture has no hvcC")
	}
	n := int(binary.BigEndian.Uint32(raw[i-4:]))
	if n < 8 || i-4+n > len(raw) {
		t.Fatal("fixture has invalid hvcC")
	}
	f, _, ok := heicConfigFrame(raw[i+4 : i-4+n])
	if !ok || f.w != 160 || f.h != 64 || f.w-f.cw <= 64 {
		t.Fatalf("fixture does not exercise the small-frame crop exception: %+v", f)
	}
	body := heicAsJPEG(provider.Responses, probeBody(raw))
	img, mime := sentImage(t, "", gjson.GetBytes(body, "input.0.content.0.image_url").Str)
	if mime != "image/jpeg" || img.Bounds().Dx() != 32 || img.Bounds().Dy() != 24 {
		t.Fatalf("small cropped frame became %s %v, want JPEG 32x24", mime, img.Bounds())
	}
}

func TestHEICConformanceCropExceptionLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
		want bool
	}{
		{"at limit", 512, 512, true},
		{"width over", 528, 64, false},
		{"height over", 64, 528, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := prefixConfig(tc.w, tc.h, tc.w-64, tc.h-64, 8)
			raw := probeHEIC(cfg, []byte{0, 0, 0, 3, 38, 1, 128}, 64, false, false)
			if got := heicWithinBudget(raw); got != tc.want {
				t.Fatalf("coded %dx%d cropped to 64x64: accepted=%t, want %t", tc.w, tc.h, got, tc.want)
			}
		})
	}
}

func TestHEICSmallConformanceCropGridBudget(t *testing.T) {
	// Every 512x512 coded tile crops to 64x64. The 32-tile control fits;
	// 40 tiles exceed twice the output pixels plus the shared allowance.
	cfg, _ := prefixConfig(512, 512, 448, 448, 8)
	for _, tc := range []struct {
		name string
		rows int
		want bool
	}{
		{"within cap", 4, true},
		{"over cap", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := probePhoneGrid(cfg, []byte{0, 0, 0, 3, 38, 1, 128}, 8*64, tc.rows*64, 64)
			if got := heicWithinBudget(raw); got != tc.want {
				t.Fatalf("%d small cropped tiles: accepted=%t, want %t", 8*tc.rows, got, tc.want)
			}
		})
	}
}

func TestHEICUUIDPropertyNumbering(t *testing.T) {
	cfg, sample := smallHEVC(t)
	raw := probeItems([]string{"hvc1"}, [][]byte{sample}, [][]byte{probeBox("uuid", make([]byte, 16)), probeISPE(64, 64), probeBox("hvcC", cfg), probeISPE(4000, 4000)}, [][]byte{{2, 3, 4}}, nil)
	if heicWithinBudget(raw) {
		t.Fatal("preflight used ispe 64 while decoder uses ispe 4000")
	}
}

func TestHEICUntiledWithinConversionBudget(t *testing.T) {
	for _, depth := range []int{8, 10} {
		cfg, _ := prefixConfig(4000, 3000, 0, 0, depth)
		raw := probeItems([]string{"hvc1"}, [][]byte{{0, 0, 0, 3, 38, 1, 128}}, [][]byte{probeBox("hvcC", cfg), probeISPE(4000, 3000)}, [][]byte{{1, 2}}, nil)
		if !heicWithinBudget(raw) {
			t.Errorf("honest untiled 12 MP depth %d was refused", depth)
		}
	}
}

func TestHEICUnusedLargeAuxiliaryConverts(t *testing.T) {
	cfg, sample := smallHEVC(t)
	large, _ := prefixConfig(7000, 7000, 0, 0, 10)
	props := [][]byte{probeBox("hvcC", cfg), probeISPE(64, 64), probeBox("hvcC", large), probeISPE(7000, 7000), probeBox("auxC", make([]byte, 4), []byte("urn:example:gainmap\x00"))}
	raw := probeItems([]string{"hvc1", "hvc1"}, [][]byte{sample, sample}, props, [][]byte{{1, 2}, {3, 4, 5}}, probeBox("auxl", []byte{0, 2, 0, 1, 0, 1}))
	body := probeBody(raw)
	if got := heicAsJPEG(provider.Responses, body); bytes.Equal(got, body) {
		t.Fatal("unused large auxiliary prevented primary conversion")
	}
}

func TestHEICTotalCodedPixelsRefused(t *testing.T) {
	// Each crop is legitimate padding, but together the tiles do four times
	// the output work. Every occurrence must count, even with a shared SPS.
	cfg, _ := prefixConfig(128, 128, 64, 64, 8)
	raw := probePhoneGrid(cfg, []byte{0, 0, 0, 3, 38, 1, 128}, 4096, 2048, 64)
	for _, repeated := range []bool{false, true} {
		name := "distinct"
		if repeated {
			name = "repeated"
			ref := bytes.Index(raw, []byte("dimg"))
			count := int(binary.BigEndian.Uint16(raw[ref+6:]))
			for i := range count {
				binary.BigEndian.PutUint16(raw[ref+8+2*i:], 2)
			}
		}
		t.Run(name, func(t *testing.T) {
			if heicWithinBudget(raw) {
				t.Fatal("coded tile work exceeded the whole-image pixel budget")
			}
		})
	}
}

func TestHEICFirstTileReferenceWins(t *testing.T) {
	cfg, sample := smallHEVC(t)
	desc := []byte{0, 0, 0, 0, 0, 64, 0, 64}
	refs := bytes.Join([][]byte{
		probeBox("dimg", []byte{0, 1, 0, 1, 0, 2}),
		probeBox("dimg", []byte{0, 1, 0, 1, 0, 3}),
	}, nil)
	raw := probeItems([]string{"grid", "hvc1", "hvc1"}, [][]byte{desc, sample, sample},
		[][]byte{probeBox("hvcC", cfg), probeISPE(64, 64)}, [][]byte{{2}, {1, 2}, {1, 2}}, refs)
	if !heicWithinBudget(raw) {
		t.Fatal("a later tile reference changed the decoder's first grid")
	}
	body := probeBody(raw)
	if bytes.Equal(heicAsJPEG(provider.Responses, body), body) {
		t.Fatal("first-reference grid did not convert")
	}
}

func TestHEICSplitExtentHeadersConvert(t *testing.T) {
	cfg, sample := smallHEVC(t)
	raw := sharedGrid(t, cfg, sample, [][2]int{{0, 1}, {1, 1}, {2, 3}, {5, len(sample) - 5}}, 1)
	body := probeBody(raw)
	if bytes.Equal(heicAsJPEG(provider.Responses, body), body) {
		t.Fatal("NAL headers split across extents did not convert")
	}
}

func TestHEICPropertyBoxFormsConvert(t *testing.T) {
	cfg, sample := smallHEVC(t)
	for _, form := range []string{"extended", "to-end", "trailing"} {
		t.Run(form, func(t *testing.T) {
			ispe := probeISPE(64, 64)
			switch form {
			case "extended":
				ispe = append(binary.BigEndian.AppendUint64([]byte{0, 0, 0, 1, 'i', 's', 'p', 'e'}, uint64(len(ispe)+8)), ispe[8:]...)
			case "to-end":
				binary.BigEndian.PutUint32(ispe, 0)
			case "trailing":
				ispe = append(ispe, 0, 0, 0)
			}
			raw := probeItems([]string{"hvc1"}, [][]byte{sample}, [][]byte{probeBox("hvcC", cfg), ispe}, [][]byte{{1, 2}}, nil)
			body := probeBody(raw)
			if bytes.Equal(heicAsJPEG(provider.Responses, body), body) {
				t.Fatal("decoder-compatible property boundaries prevented conversion")
			}
		})
	}
}

func TestHEICIgnoresAssociationsOutsideProperties(t *testing.T) {
	small, sample := smallHEVC(t)
	large, _ := prefixConfig(2048, 2048, 0, 0, 8)
	raw := probeItems([]string{"hvc1"}, [][]byte{sample},
		[][]byte{probeBox("hvcC", small), probeISPE(64, 64), probeBox("hvcC", large), probeISPE(32, 32)}, [][]byte{{3, 4}}, nil)
	// Replace the ignored handler with a same-sized, direct meta/ipma.
	// Its earlier small associations must not override the real iprp/ipma.
	i := bytes.Index(raw, []byte("hdlr"))
	n := int(binary.BigEndian.Uint32(raw[i-4:]))
	copy(raw[i:i+4], "ipma")
	clear(raw[i+4 : i-4+n])
	copy(raw[i+4:], []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 2, 1, 2})
	if heicWithinBudget(raw) {
		t.Fatal("associations outside iprp hid the decoder's large primary")
	}
}
