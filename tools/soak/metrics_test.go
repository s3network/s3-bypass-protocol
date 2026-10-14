package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- percentiles and medians --------------------------------------------------

// p90 must be a different number from the worst value. With int(n*p) as the
// index, a run of 10 transfers reports p90 == p100 — the headline number a
// reader trusts is the one outlier at the end of the run.
func TestPercentileIsNearestRankAndNotTheMaximum(t *testing.T) {
	sorted := make([]time.Duration, 10)
	for i := range sorted {
		sorted[i] = time.Duration(i+1) * time.Millisecond // 1ms .. 10ms
	}

	p90 := percentileOf(sorted, 0.90)
	worst := sorted[len(sorted)-1]
	if p90 == worst {
		t.Fatalf("p90 = %v, which is the worst value; the percentile is off by one", p90)
	}
	if p90 != sorted[8] {
		t.Fatalf("p90 = %v, want the 9th of 10 values (%v)", p90, sorted[8])
	}
	if got := percentileOf(sorted, 1.0); got != worst {
		t.Fatalf("p100 = %v, want the worst value %v", got, worst)
	}
	if got := percentileOf(sorted, 0); got != sorted[0] {
		t.Fatalf("p0 = %v, want the smallest value %v", got, sorted[0])
	}
}

func TestPercentileClampsRatherThanIndexingPastTheEnd(t *testing.T) {
	sorted := []time.Duration{time.Second}
	if got := percentileOf(sorted, 2.0); got != time.Second {
		t.Fatalf("an out-of-range percentile must clamp, got %v", got)
	}
	if got := percentileOf(nil, 0.9); got != 0 {
		t.Fatalf("no samples must be zero, got %v", got)
	}
}

// The median of an even number of samples is the mean of the middle two.
// sorted[len/2] took the upper one, so every "median" on a healthy run reported
// its own pessimistic end.
func TestMedianOfAnEvenSampleAveragesTheMiddlePair(t *testing.T) {
	sorted := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond, 40 * time.Millisecond}
	if got := medianOf(sorted); got != 25*time.Millisecond {
		t.Fatalf("median = %v, want 25ms (the mean of 20 and 30)", got)
	}
	if got := medianOf([]time.Duration{5 * time.Millisecond, 7 * time.Millisecond, 9 * time.Millisecond}); got != 7*time.Millisecond {
		t.Fatalf("median of an odd sample = %v, want 7ms", got)
	}
	if got := medianOf(nil); got != 0 {
		t.Fatalf("median of nothing = %v, want 0", got)
	}
}

func TestSummarizeUsesTheCorrectedStatistics(t *testing.T) {
	start := time.Now()
	var ts []Transfer
	for i := 1; i <= 10; i++ {
		ts = append(ts, Transfer{
			Dir: "down", Start: start,
			TTFB: time.Duration(i) * time.Millisecond, Duration: time.Second, Bytes: 100,
		})
	}
	s := summarize(ts, start, start.Add(10*time.Second), testThresholds)

	if s.DownP90TTFB == s.DownWorstTTFB {
		t.Fatalf("p90 %v equals the worst %v", s.DownP90TTFB, s.DownWorstTTFB)
	}
	if s.DownMedianTTFB != 5500*time.Microsecond {
		t.Fatalf("median = %v, want the mean of the 5th and 6th samples (5.5ms)", s.DownMedianTTFB)
	}
}

// The last slice of a run is shorter than the bucket width. Normalising it by
// the full width puts a dip at the end of every timeline, exactly where a reader
// is least likely to read it as an artefact.
func TestLastBucketIsNormalisedByItsRealDuration(t *testing.T) {
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.Local)
	ts := []Transfer{
		{Dir: "down", Start: start.Add(1 * time.Second), Bytes: 3000, TTFB: 10 * time.Millisecond},
		{Dir: "down", Start: start.Add(35 * time.Second), Bytes: 1000, TTFB: 10 * time.Millisecond},
	}
	// The run ends 10s into the second slice, which is 30s wide.
	end := start.Add(40 * time.Second)

	buckets := Bucketize(ts, start, 30*time.Second, testThresholds, end)
	if len(buckets) != 2 {
		t.Fatalf("got %d slices, want 2", len(buckets))
	}
	if got := buckets[1].Throughput; got != 100 {
		t.Fatalf("last slice throughput = %v, want 100 B/s (1000 bytes over its real 10s)", got)
	}

	// Without the run end it falls back to the full width.
	wide := Bucketize(ts, start, 30*time.Second, testThresholds)
	if got := wide[1].Throughput; got == 100 {
		t.Fatal("without a run end the slice must still use the full width")
	}
}

// --- byte accounting ----------------------------------------------------------

// The upload direction is measured by what went INTO the tunnel. Reading the
// response body — a couple of bytes from __up — reported a fraction of the real
// throughput for every upload, which is half of every figure the tool prints.
func TestUploadCountsWhatItSentNotWhatItGotBack(t *testing.T) {
	const payload = 4096

	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got = int(n)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	restoreURL := *upURL
	restoreSize := size
	*upURL = srv.URL
	size = sizeFlag(payload)
	defer func() { *upURL = restoreURL; size = restoreSize }()

	tr := doTransfer(&http.Client{}, 0, "up")
	if tr.Err != "" {
		t.Fatalf("upload failed: %s", tr.Err)
	}
	if got != payload {
		t.Fatalf("the server received %d bytes, want %d", got, payload)
	}
	if tr.Bytes != payload {
		t.Fatalf("transfer reports %d bytes; the response body was 2 and the request carried %d",
			tr.Bytes, payload)
	}
}

// A response that stops arriving mid-stream is the symptom this tool exists to
// catch. The io.Copy error used to be discarded and the transfer recorded as a
// clean success.
func TestTruncatedDownloadIsNotRecordedAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Closing without writing the promised bytes ends the body early.
	}))
	defer srv.Close()

	restoreURL, restoreSize := *downURL, size
	*downURL = srv.URL
	size = sizeFlag(64)
	defer func() { *downURL = restoreURL; size = restoreSize }()

	tr := doTransfer(&http.Client{}, 0, "down")
	if tr.Err == "" {
		t.Fatal("a body that stopped arriving must not be recorded as a success")
	}
	if !strings.Contains(tr.Err, "read body") {
		t.Fatalf("failure %q must name the truncated read", tr.Err)
	}
	if !tr.Stalled(testThresholds) {
		t.Fatal("a truncated download must count as a stall")
	}
}

func TestDownloadCountsWhatArrived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 2048))
	}))
	defer srv.Close()

	restoreURL, restoreSize := *downURL, size
	*downURL = srv.URL
	size = sizeFlag(2048)
	defer func() { *downURL = restoreURL; size = restoreSize }()

	tr := doTransfer(&http.Client{}, 0, "down")
	if tr.Err != "" {
		t.Fatalf("download failed: %s", tr.Err)
	}
	if tr.Bytes != 2048 {
		t.Fatalf("transfer reports %d bytes, want the 2048 that arrived", tr.Bytes)
	}
}

// countingReader must report the bytes the client actually handed over, which
// for a body that is cut short is less than its length.
func TestCountingReaderCountsWhatWasRead(t *testing.T) {
	cr := &countingReader{r: strings.NewReader("hello")}
	if _, err := io.Copy(io.Discard, cr); err != nil {
		t.Fatal(err)
	}
	if cr.n != 5 {
		t.Fatalf("counted %d bytes, want 5", cr.n)
	}
}

// --- flag validation ----------------------------------------------------------

// "-size -1" used to reach make([]byte, -1) and panic on the first transfer.
func TestParseSizeRejectsNegativeAndNonsense(t *testing.T) {
	for _, v := range []string{"-1", "-1MB", "-512KB"} {
		if got, err := parseSize(v); err == nil {
			t.Fatalf("parseSize(%q) = %d, want an error", v, got)
		}
	}
	if n, err := parseSize("4MB"); err != nil || n != 4<<20 {
		t.Fatalf("parseSize(4MB) = %d, %v", n, err)
	}
	if _, err := parseSize("banana"); err == nil {
		t.Fatal("parseSize must reject nonsense")
	}
}

func TestHumanBytesNeverGoesBackwards(t *testing.T) {
	if got := humanBytes(-1); !strings.HasPrefix(got, "-") {
		t.Fatalf("humanBytes(-1) = %q", got)
	}
	if got := fmt.Sprint(humanBytes(1 << 20)); got == "" {
		t.Fatal("humanBytes must say something")
	}
}
