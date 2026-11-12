package gateway

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/gen2brain/h265/hevc"
	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

// configGrid chooses a configuration for each tile, storing its sample once.
func configGrid(configs [][]byte, sample []byte, order []int, rows, tile int) []byte {
	cols := len(order) / rows
	w, h := cols*tile, rows*tile
	desc := []byte{0, 1, byte(rows - 1), byte(cols - 1)}
	desc = binary.BigEndian.AppendUint32(desc, uint32(w))
	desc = binary.BigEndian.AppendUint32(desc, uint32(h))
	types := []string{"grid"}
	data := [][]byte{desc}
	props := [][]byte{probeISPE(w, h), probeISPE(tile, tile)}
	assocs := [][]byte{{1}}
	for _, cfg := range configs {
		props = append(props, probeBox("hvcC", cfg))
	}
	refs := binary.BigEndian.AppendUint16([]byte{0, 1}, uint16(len(order)))
	for i, cfg := range order {
		types = append(types, "hvc1")
		data = append(data, sample)
		assocs = append(assocs, []byte{2, byte(cfg + 3)})
		refs = binary.BigEndian.AppendUint16(refs, uint16(i+2))
	}
	return probeItems(types, data, props, assocs, probeBox("dimg", refs))
}

func TestHEICGridCrossTilePPSRefused(t *testing.T) {
	large, nal := prefixConfig(512, 512, 504, 504, 8)
	small, _ := prefixConfig(8, 8, 0, 0, 8)
	// These are parameter and slice prefixes for preflight, not coded pixels.
	// Change the large SPS's id from ue(0) to ue(1), after profile_tier_level.
	u, ok := hevc.ParseNAL(nal)
	if !ok {
		t.Fatal("invalid SPS prefix")
	}
	rbsp := make([]byte, len(u.RBSP)+1)
	copy(rbsp, u.RBSP[:13])
	rbsp[13] = 0x40 // ue(1) = 010
	for bit := 105; bit < len(u.RBSP)*8; bit++ {
		rbsp[(bit+2)/8] |= (u.RBSP[bit/8] >> uint(7-bit%8) & 1) << uint(7-(bit+2)%8)
	}
	u.RBSP = rbsp
	nal = hevc.MarshalNAL(u)
	large = append(large[:26:26], binary.BigEndian.AppendUint16(nil, uint16(len(nal)))...)
	large = append(large, nal...)
	// PPS id 1 refers to SPS id 1; PPS id 0 refers to SPS id 0.
	large[22]++
	large = append(large, 34, 0, 1, 0, 3, 68, 1, 0x4a)
	small[22]++
	small = append(small, 34, 0, 1, 0, 3, 68, 1, 0xe0)
	// Every slice refers to PPS id 1, even on tiles whose hvcC only has id 0.
	sample := []byte{0, 0, 0, 3, 38, 1, 0x94}
	order := make([]int, 64)
	for i := 2; i < len(order); i++ {
		order[i] = 1
	}
	// Seed both workers with the large frame. Its crop equals the small frame's,
	// but decoding all 64 occurrences with the retained PPS exceeds the pixel cap.
	raw := configGrid([][]byte{large, small}, sample, order, 8, 8)
	if heicWithinBudget(raw) {
		t.Fatal("grid accepted slices that can select a larger SPS from another tile")
	}
	// The same references with uniformly small frames fit both byte and pixel caps.
	if !heicWithinBudget(configGrid([][]byte{small, small}, sample, order, 8, 8)) {
		t.Fatal("uniform small-frame control was refused")
	}
}

func TestHEICGridConfigurationReplayBudget(t *testing.T) {
	cfg, sample := smallHEVC(t)
	configs := make([][]byte, 2)
	for i := range configs {
		// One 32 KiB SEI per hvcC keeps the file small, but replaying it for
		// 32 tiles exceeds the input-sized allowance. Include escape sequences.
		sei := append([]byte{78, 1, byte(i)}, bytes.Repeat([]byte{0, 0, 3}, (32<<10)/3)...)
		c := bytes.Clone(cfg)
		c[22]++
		c = append(c, 39, 0, 1)
		c = binary.BigEndian.AppendUint16(c, uint16(len(sei)))
		configs[i] = append(c, sei...)
	}
	for _, tc := range []struct {
		name  string
		count int
		want  bool
	}{
		{"within budget", 2, true},
		{"over budget", 32, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order := make([]int, tc.count)
			for i := range order {
				order[i] = i % 2
			}
			raw := configGrid(configs, sample, order, 1, 64)
			if got := heicWithinBudget(raw); got != tc.want {
				t.Fatalf("%d alternating configurations in %d bytes: accepted=%t, want %t", tc.count, len(raw), got, tc.want)
			}
		})
	}
}

func TestHEICSharedGridConfigurationConverts(t *testing.T) {
	cfg, sample := smallHEVC(t)
	raw := configGrid([][]byte{cfg}, sample, make([]int, 16), 4, 64)
	if !heicWithinBudget(raw) {
		t.Fatal("grid sharing one small hvcC was refused")
	}
	body := heicAsJPEG(provider.Responses, probeBody(raw))
	img, mime := sentImage(t, "", gjson.GetBytes(body, "input.0.content.0.image_url").Str)
	if mime != "image/jpeg" || img.Bounds().Dx() != 256 || img.Bounds().Dy() != 256 {
		t.Fatalf("shared-configuration grid became %s %v, want JPEG 256x256", mime, img.Bounds())
	}
}
