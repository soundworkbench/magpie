package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

// probeHEVC writes a flat still with a real encoder, leaving no media in the tree.
func probeHEVC(t *testing.T, size int, tenBit bool, phone ...bool) ([]byte, []byte) {
	t.Helper()
	return probeHEVCRect(t, size, size, tenBit, phone...)
}

func probeHEVCRect(t *testing.T, width, height int, tenBit bool, phone ...bool) ([]byte, []byte) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	encoders, err := proc.CommandContext(ctx, ffmpeg, "-hide_banner", "-encoders").CombinedOutput()
	if err != nil || !bytes.Contains(encoders, []byte("libx265")) {
		t.Skip("libx265 is unavailable")
	}
	pix, profile := "yuv420p", "main"
	if tenBit {
		pix, profile = "yuv444p10le", "main444-10"
	}
	if len(phone) > 0 && phone[0] {
		pix, profile = "yuv420p10le", "main10"
	}
	path := filepath.Join(t.TempDir(), "flat.hevc")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", fmt.Sprintf("color=c=gray:s=%dx%d:r=1,format=%s", width, height, pix), "-frames:v", "1", "-c:v", "libx265", "-preset", "ultrafast", "-profile:v", profile, "-pix_fmt", pix, "-x265-params", "pools=none:frame-threads=1:wpp=0:keyint=1:repeat-headers=1:info=0:sao=1:allow-non-conformance=1", "-f", "hevc", path}
	log, err := proc.CommandContext(ctx, ffmpeg, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("encoder refused %s %dx%d: %v\n%s", profile, width, height, err, log)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Both Annex-B start code lengths occur in x265 output.
	raw = bytes.ReplaceAll(raw, []byte{0, 0, 0, 1}, []byte{0, 0, 1})
	var params, sample []byte
	count := byte(0)
	for _, nal := range bytes.Split(raw, []byte{0, 0, 1}) {
		if len(nal) < 2 {
			continue
		}
		typ := (nal[0] >> 1) & 63
		if typ >= 32 && typ <= 34 {
			params = append(params, 0x80|typ, 0, 1)
			params = binary.BigEndian.AppendUint16(params, uint16(len(nal)))
			params = append(params, nal...)
			count++
		} else {
			sample = binary.BigEndian.AppendUint32(sample, uint32(len(nal)))
			sample = append(sample, nal...)
		}
	}
	if count != 3 || len(sample) == 0 {
		t.Fatal("encoder did not produce VPS, SPS, PPS and a picture")
	}
	cfg := make([]byte, 23)
	cfg[0], cfg[21], cfg[22] = 1, 3, count
	cfg[16] = 1
	if tenBit {
		cfg[16], cfg[17], cfg[18] = 3, 2, 2
	}
	if len(phone) > 0 && phone[0] {
		cfg[16] = 1
	}
	return append(cfg, params...), sample
}

func probeBox(typ string, parts ...[]byte) []byte {
	b := make([]byte, 8)
	copy(b[4:], typ)
	for _, p := range parts {
		b = append(b, p...)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	return b
}

func probeISPE(w, h int) []byte {
	b := make([]byte, 4)
	b = binary.BigEndian.AppendUint32(b, uint32(w))
	return probeBox("ispe", binary.BigEndian.AppendUint32(b, uint32(h)))
}

// probeHEIC makes an item file, with two tiles or an auxiliary alpha when asked.
func probeHEIC(cfg, sample []byte, declared int, grid, alpha bool) []byte {
	types := []string{"hvc1"}
	data := [][]byte{sample}
	props := [][]byte{probeBox("hvcC", cfg), probeISPE(declared, declared)}
	assocs := [][]byte{{1, 2}}
	var refs []byte
	if grid {
		types = []string{"grid", "hvc1", "hvc1"}
		g := []byte{0, 0, 0, 1}
		g = binary.BigEndian.AppendUint16(g, uint16(declared))
		g = binary.BigEndian.AppendUint16(g, uint16(declared))
		data = [][]byte{g, sample, sample}
		assocs = [][]byte{{2}, {1, 2}, {1, 2}}
		refs = probeBox("dimg", []byte{0, 1, 0, 2, 0, 2, 0, 3})
	}
	if alpha {
		types = append(types, "hvc1")
		data = append(data, sample)
		props = append(props, probeBox("auxC", make([]byte, 4), []byte("urn:mpeg:hevc:2015:auxid:1\x00")))
		assocs = append(assocs, []byte{1, 2, 3})
		refs = probeBox("auxl", []byte{0, 2, 0, 1, 0, 1})
	}
	return probeItems(types, data, props, assocs, refs)
}

func probeItems(types []string, data, props, assocs [][]byte, refs []byte) []byte {
	ftyp := probeBox("ftyp", []byte("heic\x00\x00\x00\x00heicmif1"))
	meta := func(offset uint32) []byte {
		iinf := binary.BigEndian.AppendUint16(make([]byte, 4), uint16(len(types)))
		iloc := binary.BigEndian.AppendUint16([]byte{0, 0, 0, 0, 0x44, 0}, uint16(len(types)))
		ipma := binary.BigEndian.AppendUint32(make([]byte, 4), uint32(len(types)))
		for i, typ := range types {
			id := uint16(i + 1)
			infe := binary.BigEndian.AppendUint16([]byte{2, 0, 0, 0}, id)
			infe = append(infe, 0, 0)
			infe = append(infe, typ...)
			iinf = append(iinf, probeBox("infe", infe, []byte{0})...)
			iloc = binary.BigEndian.AppendUint16(iloc, id)
			iloc = append(iloc, 0, 0, 0, 1)
			iloc = binary.BigEndian.AppendUint32(iloc, offset)
			iloc = binary.BigEndian.AppendUint32(iloc, uint32(len(data[i])))
			offset += uint32(len(data[i]))
			ipma = binary.BigEndian.AppendUint16(ipma, id)
			ipma = append(ipma, byte(len(assocs[i])))
			ipma = append(ipma, assocs[i]...)
		}
		hdlr := append(make([]byte, 8), []byte("pict")...)
		hdlr = append(hdlr, make([]byte, 13)...)
		return probeBox("meta", make([]byte, 4), probeBox("hdlr", hdlr), probeBox("pitm", []byte{0, 0, 0, 0, 0, 1}), probeBox("iinf", iinf), probeBox("iloc", iloc), probeBox("iref", make([]byte, 4), refs), probeBox("iprp", probeBox("ipco", props...), probeBox("ipma", ipma)))
	}
	header := meta(0)
	header = meta(uint32(len(ftyp) + len(header) + 8))
	return bytes.Join([][]byte{ftyp, header, probeBox("mdat", data...)}, nil)
}

func probeBody(raw []byte) []byte {
	return []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/heic;base64,` + base64.StdEncoding.EncodeToString(raw) + `"}]}]}`)
}

// probePhoneGrid uses many small coded tiles, with honest output dimensions.
func probePhoneGrid(cfg, sample []byte, w, h, tile int) []byte {
	rows, cols := (h+tile-1)/tile, (w+tile-1)/tile
	desc := []byte{0, 1, byte(rows - 1), byte(cols - 1)}
	desc = binary.BigEndian.AppendUint32(desc, uint32(w))
	desc = binary.BigEndian.AppendUint32(desc, uint32(h))
	types := []string{"grid"}
	data := [][]byte{desc}
	props := [][]byte{probeBox("hvcC", cfg), probeISPE(w, h), probeISPE(tile, tile)}
	assocs := [][]byte{{2}}
	refs := binary.BigEndian.AppendUint16([]byte{0, 1}, uint16(rows*cols))
	for i := range rows * cols {
		types = append(types, "hvc1")
		data = append(data, sample)
		assocs = append(assocs, []byte{1, 3})
		refs = binary.BigEndian.AppendUint16(refs, uint16(i+2))
	}
	return probeItems(types, data, props, assocs, probeBox("dimg", refs))
}
