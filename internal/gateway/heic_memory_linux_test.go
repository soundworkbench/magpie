package gateway

import (
	"bytes"
	"crypto/sha256"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gen2brain/h265/heic"
	"github.com/yetone/magpie/internal/provider"
)

// TestHEICMemoryProbe runs one case per process so Maxrss belongs to that case.
// Set MAGPIE_HEIC_MEMORY_PROBE=1 and select a single subtest explicitly.
func TestHEICMemoryProbe(t *testing.T) {
	if os.Getenv("MAGPIE_HEIC_MEMORY_PROBE") != "1" {
		t.Skip("set MAGPIE_HEIC_MEMORY_PROBE=1 to measure")
	}
	for _, name := range []string{"A", "B", "C", "D", "E", "Phone48", "Untiled8", "Untiled10", "Limit8", "Limit10"} {
		t.Run(name, func(t *testing.T) {
			coded := 7000
			if name == "Phone48" {
				coded = 512
			}
			if name == "Untiled8" || name == "Untiled10" {
				coded = 4096
			}
			codedH := coded
			if name == "Limit8" {
				coded, codedH = 1360, 16384
			}
			if name == "Limit10" {
				coded, codedH = 1072, 16144
			}
			cfg, sample := probeHEVCRect(t, coded, codedH, name != "E" && name != "Untiled8" && name != "Limit8", name == "Phone48" || name == "Untiled10" || name == "Limit10")
			declared := 64
			if name == "B" || name == "E" {
				declared = coded
			}
			if name == "Untiled8" || name == "Untiled10" {
				declared = coded
			}
			raw := probeHEIC(cfg, sample, declared, name == "C", name == "D")
			wantW, wantH := declared, declared
			if name == "Phone48" {
				raw = probePhoneGrid(cfg, sample, 8000, 6000, 512)
				wantW, wantH = 8000, 6000
			}
			if name == "Limit8" || name == "Limit10" {
				raw = probeItems([]string{"hvc1"}, [][]byte{sample}, [][]byte{probeBox("hvcC", cfg), probeISPE(coded, codedH)}, [][]byte{{1, 2}}, nil)
				wantW, wantH = coded, codedH
			}
			if name != "Phone48" {
				f, _, ok := heicConfigFrame(cfg)
				if !ok {
					t.Fatal("cannot estimate probe configuration")
				}
				frames, sampleBytes := uint64(1), uint64(1)
				if name == "C" || name == "D" {
					frames = 2
				}
				if max(f.depthY, f.depthC) > 8 {
					sampleBytes = 2
				}
				estimate := frames*f.workBytes() + heicOutputBytes(uint64(wantW), uint64(wantH), sampleBytes, false, false) + 8*uint64(len(raw)) + (16 << 20)
				if name == "C" {
					estimate += 6 * uint64(wantW*wantH)
				}
				t.Logf("input=%d codedSPS=%dx%d conversionEstimate=%.2f MiB", len(raw), f.w, f.h, float64(estimate)/(1<<20))
			}
			c, err := heic.DecodeConfig(bytes.NewReader(raw))
			if err != nil || c.Width != wantW || c.Height != wantH {
				t.Fatalf("invalid probe header: %+v, %v", c, err)
			}
			body := probeBody(raw)
			key := sha256.Sum256(raw)
			heicCache.Lock()
			delete(heicCache.m, key)
			heicCache.Unlock()
			debug.FreeOSMemory()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			stop, done := make(chan struct{}), make(chan struct{})
			peakHeap, peakSys := before.HeapSys, before.Sys
			go func() {
				defer close(done)
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					peakHeap = max(peakHeap, m.HeapSys)
					peakSys = max(peakSys, m.Sys)
					select {
					case <-stop:
						return
					case <-ticker.C:
					}
				}
			}()
			stopSampling := sync.OnceFunc(func() { close(stop); <-done })
			defer stopSampling()
			out := heicAsJPEG(provider.Responses, body)
			// The request may time out while its conversion continues. Include that work.
			deadline := time.Now().Add(5 * time.Minute)
			for {
				heicCache.Lock()
				result, finished := heicCache.m[key]
				heicCache.Unlock()
				if finished {
					t.Logf("background conversion produced image: %t", len(result.data) > 0)
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("conversion did not finish")
				}
				time.Sleep(10 * time.Millisecond)
			}
			stopSampling()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			var usage syscall.Rusage
			if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
				t.Fatal(err)
			}
			outcome := "left as it came"
			if !bytes.Equal(body, out) {
				outcome = "converted"
			}
			t.Logf("case=%s coded=%dx%d ispe=%dx%d outcome=%s TotalAlloc=%.2f MiB peakHeapSys=%.2f MiB peakSys=%.2f MiB Maxrss=%.2f MiB", name, coded, codedH, wantW, wantH, outcome, float64(after.TotalAlloc-before.TotalAlloc)/(1<<20), float64(max(peakHeap, after.HeapSys))/(1<<20), float64(max(peakSys, after.Sys))/(1<<20), float64(usage.Maxrss)/1024)
		})
	}
}
