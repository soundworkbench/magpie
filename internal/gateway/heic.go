package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/h265/heic"

	"github.com/yetone/magpie/internal/provider"
)

// An iPhone's photos are HEIC, and an agent sends one dragged in as it is
// (Pi, Pi Desktop). Most vendors take JPEG, PNG, GIF and WebP only, and
// turn the request away: DeepSeek on WorkBuddy did (qikamatata on X).
// heicAsJPEG writes each HEIC image of a request again as JPEG, or as PNG
// when it has transparency, before anything reads the body, so every
// route has it so: a provider's own, a plugin's fetch, a describer, the
// count of its tokens. It goes by the bytes, not the type the request
// gives them: a HEIC sent as image/jpeg is one too.
//
// The decoder is gen2brain/h265's, pure Go (magpie is built without cgo
// for every system), so it is the same on every computer, and no system
// converter need be there.
//
// A request with no HEIC in it is not walked: mayHoldHEIC is a scan of
// its bytes. An image that can't be read, is too big, or takes too long
// goes as it came, and the vendor says what it says of it.

// heicBrands are the ftyp brands of an HEVC still (ISO/IEC 23008-12), as
// the decoder registers them.
var heicBrands = []string{"heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs", "mif1", "msf1"}

// heicMarks are the base64 of "yp" and a brand: the file's bytes 6 to 11,
// "ftyp" ending at 8, are its base64's characters 8 to 15 wherever the
// file's base64 starts. All begin "eXB".
var heicMarks = func() map[string]bool {
	m := map[string]bool{}
	for _, b := range heicBrands {
		m[base64.StdEncoding.EncodeToString([]byte("yp"+b))] = true
	}
	return m
}()

const (
	// a HEIC larger than this goes as it came
	heicMaxBase64 = 48 << 20
	// nor is one decoded with more pixels than a 48 MP iPhone photo
	heicMaxPixels = 50_000_000
	// past this many pixels, what a conversion held is given back at once
	heicFreeAbove = 16_000_000
	// the longest side written: no vendor reads more, and a 24 or 48 MP
	// photo written whole would be a JPEG of tens of megabytes
	heicMaxSide = 4096
	// what the cache of converted images holds at most
	heicCacheBytes   = 64 << 20
	heicCacheEntries = 32
)

// heicWait is how long a request waits for its image; one not converted
// by then goes as it came, and its conversion, finished, serves the
// request's next turn.
var heicWait = 30 * time.Second

// heicEncode writes a HEIC image again; a var for tests.
var heicEncode = encodeHEIC

// mayHoldHEIC is whether the body may have a HEIC in it: an image/heic or
// image/heif type, or the base64 of an HEVC still's ftyp.
func mayHoldHEIC(body []byte) bool {
	if bytes.Contains(body, []byte("image/hei")) || bytes.Contains(body, []byte(`image\/hei`)) {
		return true
	}
	for rest := body; ; {
		i := bytes.Index(rest, []byte("eXB"))
		if i < 0 {
			return false
		}
		if i+8 <= len(rest) && heicMarks[string(rest[i:i+8])] {
			return true
		}
		rest = rest[i+3:]
	}
}

// heicAsJPEG is the body with each HEIC image in it written as JPEG (PNG
// with transparency); the body as it came when there is none.
func heicAsJPEG(proto provider.Protocol, body []byte) []byte {
	if !mayHoldHEIC(body) {
		return body
	}
	out, err := walkImages(proto, body, func(im imageAt) (json.RawMessage, error) {
		raw := heicOf(im.Src)
		if raw == nil {
			return nil, nil
		}
		mime, data := heicConverted(raw)
		if data == nil {
			return nil, nil
		}
		return withImageData(proto, im.Block, mime, base64.StdEncoding.EncodeToString(data)), nil
	})
	if err != nil {
		return body
	}
	return out
}

// heicOf is the bytes of a data: URL's image when they are a HEIC's.
func heicOf(src string) []byte {
	rest, ok := strings.CutPrefix(src, "data:")
	if !ok {
		return nil
	}
	head, b64, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasSuffix(head, ";base64") || len(b64) < 16 || len(b64) > heicMaxBase64 {
		return nil
	}
	sniff, err := base64.StdEncoding.DecodeString(b64[:16])
	if err != nil || !isHEIC(sniff) {
		return nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(b64, "="))
	if err != nil {
		return nil
	}
	return raw
}

// isHEIC is whether b starts as an HEVC still's file does.
func isHEIC(b []byte) bool {
	if len(b) < 12 || string(b[4:8]) != "ftyp" {
		return false
	}
	for _, brand := range heicBrands {
		if string(b[8:12]) == brand {
			return true
		}
	}
	return false
}

type heicResult struct {
	mime string
	data []byte // nil: it goes as it came
}

var heicCache struct {
	sync.Mutex
	m     map[[32]byte]heicResult
	order [][32]byte
	size  int
}

// heicSlots is how many images are decoded at once: a 48 MP photo holds
// a few hundred megabytes while it is.
var heicSlots = make(chan struct{}, 1)

// heicConverted is the HEIC raw written again, from the cache when it was
// before: an agent sends its conversation's images on every turn.
func heicConverted(raw []byte) (string, []byte) {
	return converted(raw, heicEncode)
}

// converted is heicConverted for an encode: the raw written again by enc,
// from the cache when it was before.
func converted(raw []byte, enc func([]byte) (string, []byte)) (string, []byte) {
	k := sha256.Sum256(raw)
	heicCache.Lock()
	r, ok := heicCache.m[k]
	heicCache.Unlock()
	if ok {
		return r.mime, r.data
	}
	done := make(chan heicResult, 1)
	go func() {
		heicSlots <- struct{}{}
		defer func() { <-heicSlots }()
		var r heicResult
		func() {
			defer func() {
				if recover() != nil {
					r = heicResult{} // the decoder's fault on a bad file
				}
			}()
			r.mime, r.data = enc(raw)
		}()
		heicKeep(k, r)
		done <- r
	}()
	timer := time.NewTimer(heicWait)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.mime, r.data
	case <-timer.C:
		return "", nil
	}
}

// heicKeep caches r, dropping the oldest past the cache's bounds.
func heicKeep(k [32]byte, r heicResult) {
	heicCache.Lock()
	defer heicCache.Unlock()
	if heicCache.m == nil {
		heicCache.m = map[[32]byte]heicResult{}
	}
	if _, ok := heicCache.m[k]; ok || len(r.data) > heicCacheBytes {
		return
	}
	heicCache.m[k] = r
	heicCache.order = append(heicCache.order, k)
	heicCache.size += len(r.data)
	for len(heicCache.order) > heicCacheEntries || heicCache.size > heicCacheBytes {
		old := heicCache.order[0]
		heicCache.order = heicCache.order[1:]
		heicCache.size -= len(heicCache.m[old].data)
		delete(heicCache.m, old)
	}
}

// encodeHEIC decodes a HEIC, turned as it says it is shown, and writes it
// as JPEG, or as PNG when it has transparency; "", nil when it can't.
func encodeHEIC(raw []byte) (string, []byte) {
	if !heicWithinBudget(raw) {
		return "", nil
	}
	cfg, err := heic.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > heicMaxPixels {
		return "", nil
	}
	// A grid must not multiply its tile buffers by the host CPU count.
	img, err := heic.Decode(bytes.NewReader(raw), heic.Options{AutoRotate: true, FrameSizeLimit: heicMaxPixels, Threads: 2})
	if err != nil {
		return "", nil
	}
	return pictureWritten(img, cfg.Width*cfg.Height)
}

// pictureWritten is img written as JPEG, or as PNG when it has
// transparency, its longest side heicMaxSide at most; "", nil when it
// can't. pixels is the count of what the decode held, a big one's home
// going back to the system once it is written.
func pictureWritten(img image.Image, pixels int) (string, []byte) {
	if pixels > heicFreeAbove {
		// a 48 MP photo held about a gigabyte while it was written
		// again (1.6s on an M5 Pro); what it held goes back to the system
		// now, not minutes later
		defer debug.FreeOSMemory()
	}
	if b := img.Bounds(); max(b.Dx(), b.Dy()) > heicMaxSide {
		w, h := heicMaxSide, max(1, b.Dy()*heicMaxSide/b.Dx())
		if b.Dy() > b.Dx() {
			w, h = max(1, b.Dx()*heicMaxSide/b.Dy()), heicMaxSide
		}
		img = triangleResize(img, w, h)
	}
	var out bytes.Buffer
	if o, ok := img.(interface{ Opaque() bool }); ok && !o.Opaque() {
		if png.Encode(&out, img) != nil {
			return "", nil
		}
		return "image/png", out.Bytes()
	}
	if jpeg.Encode(&out, img, &jpeg.Options{Quality: 90}) != nil {
		return "", nil
	}
	return "image/jpeg", out.Bytes()
}

// withImageData is an image block with its data replaced by b64 of mime,
// its other fields as they were; nil when the block can't be read.
func withImageData(proto provider.Protocol, block json.RawMessage, mime, b64 string) json.RawMessage {
	var m map[string]json.RawMessage
	if decodeJSON(block, &m) != nil {
		return nil
	}
	set := func(field string, values map[string]string) bool {
		inner := map[string]json.RawMessage{}
		if len(m[field]) > 0 && decodeJSON(m[field], &inner) != nil {
			return false
		}
		for k, v := range values {
			inner[k], _ = json.Marshal(v)
		}
		m[field], _ = json.Marshal(inner)
		return true
	}
	url := "data:" + mime + ";base64," + b64
	switch proto {
	case provider.Chat:
		var s string
		if decodeJSON(m["image_url"], &s) == nil {
			m["image_url"], _ = json.Marshal(url)
		} else if !set("image_url", map[string]string{"url": url}) {
			return nil
		}
	case provider.Responses:
		m["image_url"], _ = json.Marshal(url)
	case provider.Anthropic:
		if !set("source", map[string]string{"type": "base64", "media_type": mime, "data": b64}) {
			return nil
		}
	case provider.Gemini:
		if !set("inlineData", map[string]string{"mimeType": mime, "data": b64}) {
			return nil
		}
	default:
		return nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

// A coded frame has planes, a SAO copy and tables even when ispe crops
// almost all of it away. Tiles have a 64 MiB work limit; an untiled
// image and its alpha share a 500 MiB conversion estimate.
const heicFrameBytes = 64 << 20

type heicBox struct {
	kind string
	data []byte
}

// heicBoxes reads only box boundaries; payloads still belong to raw.
func heicBoxes(raw []byte) ([]heicBox, bool) {
	var out []heicBox
	for count := 0; len(raw) >= 8; count++ {
		if count >= 4096 {
			return nil, false
		}
		n, head := uint64(binary.BigEndian.Uint32(raw)), 8
		if n == 1 {
			if len(raw) < 16 {
				return nil, false
			}
			n, head = binary.BigEndian.Uint64(raw[8:]), 16
		} else if n == 0 {
			n = uint64(len(raw))
		}
		if n < uint64(head) || n > uint64(len(raw)) {
			return nil, false
		}
		// The decoder skips uuid and ignores a trailing partial header.
		if string(raw[4:8]) != "uuid" {
			out = append(out, heicBox{string(raw[4:8]), raw[head:n]})
		}
		raw = raw[n:]
	}
	return out, true
}

type heicReader struct {
	data []byte
	bad  bool
}

func (r *heicReader) take(n int) []byte {
	if n < 0 || n > len(r.data) {
		r.bad = true
		return nil
	}
	b := r.data[:n]
	r.data = r.data[n:]
	return b
}
func (r *heicReader) num(n int) uint64 {
	if n < 0 || n > 8 {
		r.bad = true
		return 0
	}
	var v uint64
	for _, b := range r.take(n) {
		v = v<<8 | uint64(b)
	}
	return v
}

type heicItem struct {
	kind        string
	props       []int
	essential   []int
	unsupported bool
	extents     [][2]uint64
	method      uint64
	located     bool
	tiles       []uint64
}

type heicHeader struct {
	items   map[uint64]*heicItem
	props   []heicProperty
	refs    []heicReference
	alphas  map[uint64]*heicItem
	budget  uint64
	idat    []byte
	primary uint64
	entries uint64
}

type heicProperty struct {
	heicBox
	frame  heicFrame
	length int
	size   [2]uint64
	alpha  bool
}

type heicReference struct {
	kind string
	from uint64
	to   []uint64
}

// spend counts logical bytes even when an extent aliases an earlier one.
func (h *heicHeader) spend(n uint64) bool {
	if n > h.budget {
		return false
	}
	h.budget -= n
	return true
}

// Header lists are bounded before DecodeConfig expands them into Go objects.
func (h *heicHeader) addEntries(n uint64) bool {
	if n > 65536-h.entries {
		return false
	}
	h.entries += n
	return true
}

func (h *heicHeader) item(id uint64) *heicItem {
	if h.items[id] == nil {
		h.items[id] = &heicItem{}
	}
	return h.items[id]
}

func (h *heicHeader) read(box heicBox) bool {
	r := heicReader{data: box.data}
	if box.kind == "idat" {
		h.idat = box.data
		return true
	}
	if box.kind == "iprp" {
		boxes, ok := heicBoxes(box.data)
		if !ok {
			return false
		}
		for _, b := range boxes {
			switch b.kind {
			case "ipco":
				props, ok := heicBoxes(b.data)
				if !ok || len(h.props) != 0 {
					return false
				}
				for _, box := range props {
					p := heicProperty{heicBox: box}
					switch p.kind {
					case "hvcC":
						// Allow for the SPS prefix scan, unescape and parse.
						if !h.spend(3 * uint64(len(p.data))) {
							return false
						}
						var ok bool
						p.frame, p.length, ok = heicConfigFrame(p.data)
						if !ok {
							return false
						}
					case "ispe":
						if len(p.data) < 12 {
							return false
						}
						p.size = [2]uint64{uint64(binary.BigEndian.Uint32(p.data[4:])), uint64(binary.BigEndian.Uint32(p.data[8:]))}
					case "auxC":
						if len(p.data) < 5 {
							return false
						}
						end := bytes.IndexByte(p.data[4:], 0)
						if end < 0 {
							return false
						}
						urn := string(p.data[4 : 4+end])
						p.alpha = urn == "urn:mpeg:mpegB:cicp:systems:auxiliary:alpha" || urn == "urn:mpeg:hevc:2015:auxid:1"
					}
					h.props = append(h.props, p)
				}
			case "ipma":
				if !h.read(b) {
					return false
				}
			}
		}
		return true
	}
	version := r.num(1)
	flags := r.num(3)
	switch box.kind {
	case "pitm":
		if version > 1 {
			return false
		}
		h.primary = r.num(2 + 2*int(version))
	case "iinf":
		n := 2
		if version != 0 {
			n = 4
		}
		r.num(n)
		boxes, ok := heicBoxes(r.data)
		if !ok {
			return false
		}
		for _, b := range boxes {
			if b.kind != "infe" {
				continue
			}
			ir := heicReader{data: b.data}
			v := ir.num(1)
			ir.num(3)
			if v != 2 && v != 3 {
				return false
			}
			it := h.item(ir.num(2 + 2*int(v-2)))
			ir.num(2)
			it.kind = string(ir.take(4))
			if ir.bad {
				return false
			}
		}
	case "iloc":
		if version > 2 {
			return false
		}
		sizes, baseSizes := r.num(1), r.num(1)
		offSize, lenSize, baseSize, idxSize := int(sizes>>4), int(sizes&15), int(baseSizes>>4), int(baseSizes&15)
		if version == 0 {
			idxSize = 0
		}
		for _, n := range []int{offSize, lenSize, baseSize, idxSize} {
			if n != 0 && n != 4 && n != 8 {
				return false
			}
		}
		idSize := 2
		if version == 2 {
			idSize = 4
		}
		count := r.num(idSize)
		if count > 4096 {
			return false
		}
		for range count {
			it := h.item(r.num(idSize))
			if it.located {
				return false
			}
			it.located = true
			if version > 0 {
				it.method = r.num(2) & 15
			}
			if r.num(2) != 0 || it.method > 1 {
				return false
			}
			base := r.num(baseSize)
			n := r.num(2)
			if n > 64 || !h.addEntries(n) {
				return false
			}
			for range n {
				r.num(idxSize)
				off, length := r.num(offSize), r.num(lenSize)
				if off+base < base {
					return false
				}
				it.extents = append(it.extents, [2]uint64{off + base, length})
			}
		}
	case "ipma":
		if version > 1 {
			return false
		}
		count := r.num(4)
		if count > 4096 {
			return false
		}
		for range count {
			it := h.item(r.num(2 + 2*int(version)))
			n := r.num(1)
			if n+uint64(len(it.props)) > 64 || !h.addEntries(n) {
				return false
			}
			for range n {
				size, mask := 1, uint64(127)
				if flags&1 != 0 {
					size, mask = 2, 32767
				}
				v := r.num(size)
				p := v & mask
				if p != 0 {
					it.props = append(it.props, int(p)-1)
					if v&(mask+1) != 0 {
						it.essential = append(it.essential, int(p)-1)
					}
				}
			}
		}
	case "iref":
		if version > 1 {
			return false
		}
		refs, ok := heicBoxes(r.data)
		if !ok || !h.addEntries(uint64(len(refs))) {
			return false
		}
		for _, ref := range refs {
			rr := heicReader{data: ref.data}
			from := rr.num(2 + 2*int(version))
			refInfo := heicReference{kind: ref.kind, from: from}
			n := rr.num(2)
			if n > 4096 || !h.addEntries(n) {
				return false
			}
			for range n {
				tid := rr.num(2 + 2*int(version))
				refInfo.to = append(refInfo.to, tid)
			}
			if rr.bad {
				return false
			}
			h.refs = append(h.refs, refInfo)
		}
	}
	return !r.bad && len(h.items) <= 4096
}

func (h *heicHeader) prop(it *heicItem, kind string) *heicProperty {
	for _, i := range it.props {
		if i >= 0 && i < len(h.props) && h.props[i].kind == kind {
			return &h.props[i]
		}
	}
	return nil
}

func (h *heicHeader) alphaOf(id uint64) *heicItem {
	return h.alphas[id]
}

// heicData walks extents in order, including headers split across extents.
// Only small prefixes are copied; skipped NAL payloads stay in the file.
type heicData struct {
	parts [][]byte
	left  uint64
}

func (h *heicHeader) data(it *heicItem, raw []byte) (heicData, bool) {
	if it.method == 1 {
		raw = h.idat
	}
	var d heicData
	for _, e := range it.extents {
		off, n := e[0], e[1]
		if off > uint64(len(raw)) {
			return d, false
		}
		if n == 0 {
			n = uint64(len(raw)) - off
		}
		if n > uint64(len(raw))-off || !h.spend(3*n) {
			return d, false
		}
		d.parts = append(d.parts, raw[off:off+n])
		d.left += n
	}
	return d, d.left > 0
}

func (d *heicData) read(dst []byte) bool {
	if uint64(len(dst)) > d.left {
		return false
	}
	d.left -= uint64(len(dst))
	for len(dst) > 0 {
		n := copy(dst, d.parts[0])
		dst = dst[n:]
		d.parts[0] = d.parts[0][n:]
		if len(d.parts[0]) == 0 {
			d.parts = d.parts[1:]
		}
	}
	return true
}

func (d *heicData) skip(n uint64) {
	d.left -= n
	for n > 0 {
		k := min(n, uint64(len(d.parts[0])))
		d.parts[0] = d.parts[0][k:]
		n -= k
		if len(d.parts[0]) == 0 {
			d.parts = d.parts[1:]
		}
	}
}

type heicFrame struct{ w, h, cw, ch, chroma, depthY, depthC uint64 }

// heicBits reads the SPS prefix only. A truncated or overflowing code is
// unknown, so the image goes as it came before any plane is allocated.
type heicBits struct {
	data []byte
	pos  int
	bad  bool
}

func (b *heicBits) bits(n int) uint64 {
	if n < 0 || n > 64 || n > len(b.data)*8-b.pos {
		b.bad = true
		return 0
	}
	var v uint64
	for range n {
		v = v<<1 | uint64(b.data[b.pos/8]>>(7-b.pos%8)&1)
		b.pos++
	}
	return v
}
func (b *heicBits) skip(n int) {
	for n > 0 {
		k := min(n, 64)
		b.bits(k)
		n -= k
	}
}
func (b *heicBits) ue() uint64 {
	n := 0
	for !b.bad && b.bits(1) == 0 {
		n++
		if n > 31 {
			b.bad = true
			return 0
		}
	}
	if b.bad {
		return 0
	}
	return (uint64(1) << n) - 1 + b.bits(n)
}

func heicSPS(nal []byte) (heicFrame, bool) {
	// The fields below fit in 256 encoded bytes even with six sublayers.
	// Match ParseNAL's unescape, but neither scan nor allocate for its tail.
	nal = nal[:min(len(nal), 256)]
	if len(nal) < 2 || nal[0]&128 != 0 || nal[1]&7 == 0 || nal[0]>>1&63 != 33 {
		return heicFrame{}, false
	}
	var rbsp [256]byte
	n := 0
	for i := 2; i < len(nal); {
		if i+2 < len(nal) && nal[i] == 0 && nal[i+1] == 0 && nal[i+2] == 3 {
			rbsp[n], rbsp[n+1] = 0, 0
			n += 2
			i += 3
		} else {
			rbsp[n] = nal[i]
			n++
			i++
		}
	}
	b := heicBits{data: rbsp[:n]}
	b.bits(4)
	layers := int(b.bits(3))
	b.bits(1)
	if layers > 6 {
		return heicFrame{}, false
	}
	b.skip(96)
	var profile, level [7]uint64
	for i := range layers {
		profile[i], level[i] = b.bits(1), b.bits(1)
	}
	if layers > 0 {
		b.skip(2 * (8 - layers))
	}
	for i := range layers {
		if profile[i] != 0 {
			b.skip(88)
		}
		if level[i] != 0 {
			b.skip(8)
		}
	}
	if b.ue() > 15 {
		return heicFrame{}, false
	}
	f := heicFrame{chroma: b.ue()}
	if f.chroma > 3 {
		return f, false
	}
	if f.chroma == 3 && b.bits(1) != 0 {
		return f, false
	}
	f.w, f.h = b.ue(), b.ue()
	if f.w == 0 || f.h == 0 || f.w > 16384 || f.h > 16384 {
		return f, false
	}
	f.cw, f.ch = f.w, f.h
	if b.bits(1) != 0 {
		left, right, top, bottom := b.ue(), b.ue(), b.ue(), b.ue()
		sw, sh := uint64(1), uint64(1)
		if f.chroma == 1 || f.chroma == 2 {
			sw = 2
		}
		if f.chroma == 1 {
			sh = 2
		}
		dx, dy := (left+right)*sw, (top+bottom)*sh
		if dx >= f.w || dy >= f.h {
			return f, false
		}
		f.cw, f.ch = f.w-dx, f.h-dy
	}
	f.depthY, f.depthC = 8+b.ue(), 8+b.ue()
	return f, !b.bad && f.depthY <= 16 && f.depthC <= 16
}

func (f heicFrame) workBytes() uint64 {
	// The decoder uses two-byte planes if either channel exceeds eight bits.
	sample := uint64(1)
	if max(f.depthY, f.depthC) > 8 {
		sample = 2
	}
	pixels := f.w * f.h
	chroma := pixels * 2
	switch f.chroma {
	case 0:
		chroma = 0
	case 1:
		chroma = pixels / 2
	case 2:
		chroma = pixels
	}
	// Eight bytes per luma sample leaves room for the CTU, motion and scan
	// tables; SAO holds one more luma plane, reused for the other channels.
	return (2*pixels+chroma)*sample + 8*pixels
}

func heicConfigFrame(cfg []byte) (heicFrame, int, bool) {
	if len(cfg) < 23 || cfg[0] != 1 {
		return heicFrame{}, 0, false
	}
	r := heicReader{data: cfg[23:]}
	var frame heicFrame
	seen := false
	count := uint64(0)
	for range int(cfg[22]) {
		r.num(1)
		n := r.num(2)
		count += n
		if count > 64 {
			return frame, 0, false
		}
		for range n {
			nal := r.take(int(r.num(2)))
			if len(nal) < 2 {
				return frame, 0, false
			}
			typ := nal[0] >> 1 & 63
			switch typ {
			case 32, 34, 39, 40:
				continue
			case 33:
			default:
				return frame, 0, false
			}
			f, ok := heicSPS(nal)
			if !ok || seen && f != frame {
				return frame, 0, false
			}
			frame, seen = f, true
		}
	}
	return frame, int(cfg[21]&3) + 1, seen && !r.bad
}

// frame checks only an item the decoder reaches. Configuration parsing is
// cached by property, while each decoded occurrence pays for its data and config.
func (h *heicHeader) frame(it *heicItem, raw []byte, tile bool) (heicFrame, bool) {
	if it == nil || it.kind != "hvc1" && it.kind != "hvc2" {
		return heicFrame{}, false
	}
	p, cfg := h.prop(it, "ispe"), h.prop(it, "hvcC")
	if p == nil || cfg == nil {
		return heicFrame{}, false
	}
	// Each grid worker re-feeds hvcC when its configuration pointer changes.
	// Charge every occurrence to cover either worker's tile order and first use.
	if !h.spend(uint64(len(cfg.data))) {
		return heicFrame{}, false
	}
	f := cfg.frame
	if p.size != [2]uint64{f.cw, f.ch} || f.w*f.h > heicMaxPixels || tile && f.workBytes() > heicFrameBytes {
		return f, false
	}
	// Small frames may crop more than coding padding. Grids require identical
	// full frames, so retained parameter sets cannot change the coded size that
	// each tile contributes to the aggregate coded-pixel cap.
	small := f.w <= 512 && f.h <= 512 && f.w*f.h <= 262144
	if !small && (f.w-f.cw > 64 || f.h-f.ch > 64) {
		return f, false
	}
	d, ok := h.data(it, raw)
	if !ok {
		return f, false
	}
	pictures := 0
	for d.left > 0 {
		var size [4]byte
		if !d.read(size[4-cfg.length:]) {
			return f, false
		}
		n := uint64(binary.BigEndian.Uint32(size[:]))
		if n < 2 || n > d.left {
			return f, false
		}
		var prefix [256]byte
		k := min(n, uint64(len(prefix)))
		if !d.read(prefix[:k]) {
			return f, false
		}
		d.skip(n - k)
		typ := prefix[0] >> 1 & 63
		if typ == 33 {
			other, ok := heicSPS(prefix[:k])
			if !ok || other != f {
				return f, false
			}
		}
		if typ < 32 {
			if n < 3 {
				return f, false
			}
			if prefix[2]&128 != 0 {
				pictures++
				if pictures > 1 || typ < 16 || typ > 23 {
					return f, false
				}
			}
		}
	}
	return f, pictures == 1
}

// outputBytes includes the full RGB output and triangleResize's RGBA source,
// horizontal float32 buffer and destination. Cropping cannot enlarge output;
// with a clean aperture, use the largest possible intermediate rectangle.
func heicOutputBytes(w, ht, sample uint64, crop, rotate bool) uint64 {
	pixels := w * ht
	total := 4 * sample * pixels
	if max(w, ht) <= heicMaxSide || crop {
		// Without resizing, JPEG takes the generic NRGBA/NRGBA64 At path.
		// Its color values escape once per pixel, including MCU padding.
		jw, jh := min(w, uint64(heicMaxSide)), min(ht, uint64(heicMaxSide))
		total += 4 * sample * ((jw + 15) / 16 * 16) * ((jh + 15) / 16 * 16)
	}
	if crop {
		total += 4 * sample * pixels
	}
	if rotate {
		total += 4 * sample * pixels
	}
	if max(w, ht) > heicMaxSide {
		tw, th := uint64(heicMaxSide), max(uint64(1), ht*heicMaxSide/w)
		if ht > w {
			tw, th = max(uint64(1), w*heicMaxSide/ht), heicMaxSide
		}
		if crop {
			tw, th = min(w, uint64(heicMaxSide)), min(ht, uint64(heicMaxSide))
		}
		total += 4*pixels + 16*tw*ht + 4*tw*th
	}
	return total
}

// heicWithinBudget bounds metadata, repeated extent work and decoded pixels
// before DecodeConfig. Honest tiled photos retain the output pixel limit;
// untiled images and their alpha also share a conversion allocation estimate.
func heicWithinBudget(raw []byte) bool {
	if len(raw) > heicMaxBase64*3/4 {
		return false
	}
	boxes, ok := heicBoxes(raw)
	if !ok {
		return false
	}
	for _, box := range boxes {
		if box.kind == "moov" {
			return false
		}
	}
	// One input-sized pass pays for box boundaries and metadata fields. The
	// remaining three passes plus 64 KiB pay for configuration and extent work.
	h := heicHeader{items: map[uint64]*heicItem{}, budget: 3*uint64(len(raw)) + (64 << 10)}
	found := false
	for _, box := range boxes {
		if box.kind != "meta" {
			continue
		}
		if len(box.data) < 4 {
			return false
		}
		children, ok := heicBoxes(box.data[4:])
		if !ok {
			return false
		}
		for _, child := range children {
			// ipma is meaningful only inside iprp. Other placements must
			// not give the preflight associations the decoder never sees.
			switch child.kind {
			case "pitm", "iinf", "iloc", "iref", "idat", "iprp":
				if !h.read(child) {
					return false
				}
			}
		}
		found = true
		break
	}
	primary := h.items[h.primary]
	if !found || primary == nil {
		return false
	}
	for _, it := range h.items {
		for _, i := range it.essential {
			if i >= len(h.props) {
				it.unsupported = true
				break
			}
			switch h.props[i].kind {
			case "ispe", "hvcC", "pixi", "auxC", "irot", "imir", "clap", "colr":
			default:
				it.unsupported = true
			}
		}
	}
	// Resolve ordered references once. refsTo uses the first dimg list;
	// alphaOf uses the first supported alpha whose first target matches.
	h.alphas = make(map[uint64]*heicItem)
	seen := make(map[uint64]bool)
	for _, r := range h.refs {
		it := h.items[r.from]
		if it == nil {
			continue
		}
		if r.kind == "dimg" && !seen[r.from] {
			it.tiles = r.to
			seen[r.from] = true
		}
		if r.kind == "auxl" && len(r.to) > 0 && h.alphas[r.to[0]] == nil && !it.unsupported {
			if p := h.prop(it, "auxC"); p != nil && p.alpha {
				h.alphas[r.to[0]] = it
			}
		}
	}
	p := h.prop(primary, "ispe")
	if primary.unsupported || p == nil {
		return false
	}
	w, ht := p.size[0], p.size[1]
	if w == 0 || ht == 0 || w > heicMaxPixels || ht > heicMaxPixels || w*ht > heicMaxPixels {
		return false
	}
	coded, work, sample := uint64(0), uint64(0), uint64(1)
	addFrame := func(it *heicItem, tile bool) (heicFrame, bool) {
		f, ok := h.frame(it, raw, tile)
		if !ok {
			return f, false
		}
		coded += f.w * f.h
		work += f.workBytes()
		if max(f.depthY, f.depthC) > 8 {
			sample = 2
		}
		// Count repeated tile IDs each time: the decoder decodes each occurrence.
		return f, coded <= 2*w*ht+(8<<20)
	}
	grid := func(it *heicItem, alpha bool) bool {
		d, ok := h.data(it, raw)
		if !ok {
			return false
		}
		var desc [12]byte
		if !d.read(desc[:4]) || desc[0] != 0 {
			return false
		}
		n := 2
		if desc[1]&1 != 0 {
			n = 4
		}
		if !d.read(desc[4 : 4+2*n]) {
			return false
		}
		r := heicReader{data: desc[4 : 4+2*n]}
		gw, gh := r.num(n), r.num(n)
		rows, cols := uint64(desc[2])+1, uint64(desc[3])+1
		gp := h.prop(it, "ispe")
		if gp == nil || gp.size != [2]uint64{gw, gh} || gw == 0 || gh == 0 || gw > heicMaxPixels || gh > heicMaxPixels || gw*gh > heicMaxPixels || uint64(len(it.tiles)) != rows*cols {
			return false
		}
		tiles := make([]*heicItem, len(it.tiles))
		for i, id := range it.tiles {
			tiles[i] = h.items[id]
			if alpha {
				tiles[i] = h.alphaOf(id)
				if tiles[i] == nil {
					return true
				} // gridAlpha decodes nothing unless all have alpha.
			}
		}
		var first heicFrame
		for i, tile := range tiles {
			f, ok := addFrame(tile, true)
			if !ok {
				return false
			}
			if i == 0 {
				first = f
			} else if f != first {
				// Workers retain SPS/PPS from earlier tiles. Any SPS they can
				// select must describe the full frame charged for this tile.
				return false
			}
		}
		return gw > first.cw*(cols-1) && gw <= first.cw*cols && gh > first.ch*(rows-1) && gh <= first.ch*rows
	}
	if primary.kind == "grid" {
		return grid(primary, false) && grid(primary, true)
	}
	if _, ok := addFrame(primary, false); !ok {
		return false
	}
	if a := h.alphaOf(h.primary); a != nil {
		if a.kind == "grid" {
			if !grid(a, false) {
				return false
			}
			// Stitched alpha planes are additional to each tile's decode work.
			ap := h.prop(a, "ispe")
			work += 6 * ap.size[0] * ap.size[1]
		} else if _, ok := addFrame(a, false); !ok {
			return false
		}
	}
	crop := h.prop(primary, "clap") != nil
	rotate := false
	if r := h.prop(primary, "irot"); r != nil {
		if len(r.data) < 1 {
			return false
		}
		rotate = r.data[0]&3 != 0
		if r.data[0]&1 != 0 {
			w, ht = ht, w
		}
	}
	// The fixed allowance covers headers, row rounding, filter weights,
	// encoder buffers and the small allocations around the conversion.
	return work+heicOutputBytes(w, ht, sample, crop, rotate)+8*uint64(len(raw))+(16<<20) <= 500<<20
}
