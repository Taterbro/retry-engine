package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ── Config ────────────────────────────────────────────────────────────────────

const (
	mockPort   = 9090
	enginePort = 8080
	backoffMs  = 500 // short so demo finishes in ~5 s
	maxRetries = 5
	failFirst  = 3 // mock returns 500 this many times, then 200
	lineWidth  = 66
)

// ── Mock server ───────────────────────────────────────────────────────────────

var hitCount int64 // accessed atomically

// startMock registers a handler that fails the first `failFirst` requests
// with 500, then returns 200 on every subsequent call.
func startMock() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hitCount, 1)
		ts := time.Now().Format("15:04:05.000")

		if n <= failFirst {
			body := fmt.Sprintf("attempt %d: intentional failure", n)
			fmt.Printf("  [mock %s]  #%d → 500  (failure %d/%d)\n", ts, n, n, failFirst)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, body)
		} else {
			body := fmt.Sprintf("attempt %d: OK — all done!", n)
			fmt.Printf("  [mock %s]  #%d → 200  SUCCESS ✅\n", ts, n)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, body)
		}
	})

	go func() {
		if err := http.ListenAndServe(fmt.Sprintf(":%d", mockPort), mux); err != nil {
			fmt.Fprintln(os.Stderr, "mock server:", err)
		}
	}()
}

// ── Port readiness ────────────────────────────────────────────────────────────

func waitForPort(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// ── API types (mirrors the engine's JSON) ─────────────────────────────────────

type createResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type attempt struct {
	AttemptNum  int     `json:"attemptNum"`
	StatusCode  *int    `json:"statusCode"`
	Error       *string `json:"error"`
	Response    *string `json:"response"`
	AttemptedAt string  `json:"attemptedAt"`
}

type requestState struct {
	ID           string    `json:"id"`
	Status       string    `json:"status"`
	AttemptCount int       `json:"attemptCount"`
	Result       *string   `json:"result"`
	Attempts     []attempt `json:"attempts"`
}

// ── Engine API calls ──────────────────────────────────────────────────────────

func submitRequest() (createResp, error) {
	payload := map[string]any{
		"url":        fmt.Sprintf("http://localhost:%d/test", mockPort),
		"method":     "GET",
		"maxRetries": maxRetries,
		"backoffMs":  backoffMs,
	}
	b, _ := json.Marshal(payload)
	resp, err := http.Post(
		fmt.Sprintf("http://localhost:%d/request", enginePort),
		"application/json",
		bytes.NewReader(b),
	)
	if err != nil {
		return createResp{}, err
	}
	defer resp.Body.Close()
	var out createResp
	json.NewDecoder(resp.Body).Decode(&out)
	return out, nil
}

func pollRequest(id string) (*requestState, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/requests/%s", enginePort, id))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out requestState
	json.NewDecoder(resp.Body).Decode(&out)
	return &out, nil
}

// ── Display helpers ───────────────────────────────────────────────────────────

func hr(ch string) { fmt.Println(strings.Repeat(ch, lineWidth)) }

// jitterBar renders a 24-char block bar mapping jitter [0.8, 1.2] → [0%, 100%].
func jitterBar(jitter float64) string {
	const bars = 24
	fill := int((jitter - 0.8) / 0.4 * float64(bars))
	if fill < 0 {
		fill = 0
	} else if fill > bars {
		fill = bars
	}
	return "[" + strings.Repeat("█", fill) + strings.Repeat("░", bars-fill) + "]"
}

// strPtr returns the string a pointer points to, or "" if nil.
func strPtr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	hr("━")
	fmt.Println("  ✦  RETRY ENGINE DEMO  (Go)")
	hr("━")
	fmt.Printf("  Mock target  │  http://localhost:%d  (500×%d, then 200)\n", mockPort, failFirst)
	fmt.Printf("  backoffMs    │  %dms → doubles each retry: %d, %d, %d …\n",
		backoffMs, backoffMs, backoffMs*2, backoffMs*4)
	fmt.Println("  jitter       │  ×[0.80 … 1.20) applied to each wait")
	fmt.Printf("  maxRetries   │  %d\n", maxRetries)
	hr("━")
	fmt.Println()

	// ── 1. Mock server ────────────────────────────────────────────────────────
	fmt.Printf("  ▶  Starting mock server on :%d …\n", mockPort)
	startMock()
	if !waitForPort(mockPort, 5*time.Second) {
		fmt.Fprintln(os.Stderr, "ERROR: mock server did not start")
		os.Exit(1)
	}

	// ── 2. Retry engine ───────────────────────────────────────────────────────
	// Resolve engine binary to an absolute path so it isn't affected
	// by engine.Dir (which changes the child's cwd before exec).
	self, _ := os.Executable()
	selfDir := filepath.Dir(self)

	// Check same dir first, then parent dir. Handle Windows .exe extension.
	candidates := []string{
		filepath.Join(selfDir, "retryengine.exe"),
		filepath.Join(selfDir, "retryengine"),
		filepath.Join(selfDir, "..", "retryengine.exe"),
		filepath.Join(selfDir, "..", "retryengine"),
	}
	engineBin := ""
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			engineBin = c
			break
		}
	}
	if engineBin == "" {
		fmt.Fprintln(os.Stderr, "ERROR: retryengine binary not found next to demo binary")
		os.Exit(1)
	}

	self, _ = os.Executable()
	workdir := filepath.Join(filepath.Dir(self), "data")
	os.MkdirAll(workdir, 0755)

	fmt.Printf("  ▶  Starting retry engine on :%d …\n", enginePort)
	engine := exec.Command(engineBin)
	engine.Dir = workdir
	engine.Stderr = io.Discard
	engine.Stdout = io.Discard
	if err := engine.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: could not start engine:", err)
		os.Exit(1)
	}
	defer engine.Process.Kill()

	if !waitForPort(enginePort, 8*time.Second) {
		fmt.Fprintln(os.Stderr, "ERROR: engine did not start")
		os.Exit(1)
	}
	fmt.Println("  ✓  Both servers ready.")

	// ── 3. Submit a request ───────────────────────────────────────────────────
	created, err := submitRequest()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: submit:", err)
		os.Exit(1)
	}
	fmt.Printf("  → Request ID     %s\n", created.ID)
	fmt.Printf("  → Initial status %s\n\n", created.Status)
	fmt.Println("  Mock server log (live, as the worker fires each attempt):")
	hr("─")
	fmt.Println()

	// ── 4. Poll until terminal state ──────────────────────────────────────────
	t0 := time.Now()
	var state *requestState
	for {
		time.Sleep(400 * time.Millisecond)
		state, err = pollRequest(created.ID)
		if err != nil {
			continue
		}
		if state.Status == "completed" || state.Status == "failed" {
			break
		}
		if time.Since(t0) > 60*time.Second {
			fmt.Fprintln(os.Stderr, "ERROR: timed out waiting for completion")
			os.Exit(1)
		}
	}
	elapsed := time.Since(t0)

	// ── 5. Formatted attempt history ──────────────────────────────────────────
	fmt.Println()
	hr("━")
	fmt.Println("  ATTEMPT HISTORY  —  backoff · jitter · outcome")
	hr("━")
	fmt.Println()
	fmt.Printf("  %-4s  %-15s  %-6s  %s\n", "#", "wall clock", "code", "detail")
	fmt.Println("  " + strings.Repeat("─", 60))

	for i, a := range state.Attempts {
		ts, _ := time.Parse(time.RFC3339Nano, a.AttemptedAt)
		tsStr := ts.Local().Format("15:04:05.000")

		codeStr := "—"
		if a.StatusCode != nil {
			codeStr = fmt.Sprintf("%d", *a.StatusCode)
		}

		icon := "❌"
		detail := strPtr(a.Error)
		if a.StatusCode != nil && *a.StatusCode < 400 {
			icon = "✅"
			detail = strPtr(a.Response)
		}
		if len(detail) > 52 {
			detail = detail[:52]
		}

		fmt.Printf("  %s  %-4d  %-15s  %-6s  %s\n", icon, a.AttemptNum, tsStr, codeStr, detail)

		// Gap line: show actual wait, base wait, and derived jitter factor.
		if i+1 < len(state.Attempts) {
			nextTs, _ := time.Parse(time.RFC3339Nano, state.Attempts[i+1].AttemptedAt)
			actualMs := float64(nextTs.Sub(ts).Milliseconds())
			baseMs := float64(backoffMs) * math.Pow(2, float64(a.AttemptNum-1))
			jitter := actualMs / baseMs

			fmt.Printf("       │  waited %6.0fms   base(%dms × 2^%d = %4.0fms)  ×%.3f jitter\n",
				actualMs, backoffMs, a.AttemptNum-1, baseMs, jitter)
			fmt.Printf("       │  jitter %s  0.80──1.20\n\n", jitterBar(jitter))
		}
	}

	hr("━")
	statusLabel := "✅  COMPLETED"
	if state.Status == "failed" {
		statusLabel = "💀  FAILED (dead-lettered)"
	}
	fmt.Printf("  Final status  │  %s\n", statusLabel)
	fmt.Printf("  Attempts used │  %d / %d\n", state.AttemptCount, maxRetries)
	fmt.Printf("  Total time    │  %.2fs\n", elapsed.Seconds())
	if state.Result != nil {
		r := *state.Result
		if len(r) > 70 {
			r = r[:70]
		}
		fmt.Printf("  Final body    │  %s\n", r)
	}
	hr("━")
	fmt.Println()
}
