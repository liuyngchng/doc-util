// Command server is a headless HTTP server that converts PDF to DOCX.
//
// Build (Docker, with OCR):
//
//	./build_server.sh
//
// The server exposes four endpoints:
//
//	POST /convert/image  → screenshot DOCX (always works)
//	POST /convert/text   → text-layer DOCX (extract text for native PDFs, OCR for scans)
//	POST /convert/doc    → .doc to .docx conversion (via LibreOffice)
//	POST /extract/txt    → plain text string (PDF or DOCX input; OCR for image-only content)
//	DELETE /cache/clear  → clear the persistent result cache
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"docutil/internal/cache"
	"docutil/internal/doc"
	"docutil/internal/ocr"
	"docutil/internal/pdf"
	"docutil/internal/slogutil"
)

// resultCache is the process-wide result cache. It is initialised in main and
// remains nil only if initialisation failed (which is a fatal startup error).
var resultCache *cache.Cache

func main() {
	// Default handler writes to stderr so docker logs works.
	logWriters := []io.Writer{os.Stderr}

	// If LOG_DIR is set, also write to a daily rotating log file. The previous
	// day's log is gzip-compressed as server.log.<date>.gz on rotation.
	if logDir := os.Getenv("LOG_DIR"); logDir != "" {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: cannot create LOG_DIR %s: %v\n", logDir, err)
		} else {
			logFile, err := slogutil.NewDailyWriter(logDir, "server.log")
			if err != nil {
				fmt.Fprintf(os.Stderr, "WARN: cannot open log file in %s: %v\n", logDir, err)
			} else {
				defer logFile.Close()
				logWriters = append(logWriters, logFile)
				fmt.Fprintf(os.Stderr, "Logging to %s (daily rotation, gzip-compressed)\n",
					filepath.Join(logDir, "server.log"))
			}
		}
	}

	slogger := slog.New(slogutil.NewHandler(io.MultiWriter(logWriters...), &slog.HandlerOptions{
		Level: slogLevel(),
	}))
	slog.SetDefault(slogger)

	// ── Result cache ────────────────────────────────────────────────────
	cacheDB := os.Getenv("CACHE_DB")
	if cacheDB == "" {
		cacheDB = "cache.db"
	}
	if !filepath.IsAbs(cacheDB) {
		if exe, err := os.Executable(); err == nil {
			cacheDB = filepath.Join(filepath.Dir(exe), cacheDB)
		}
	}

	cacheMaxMB, _ := strconv.Atoi(os.Getenv("CACHE_MAX_MB"))
	if cacheMaxMB <= 0 {
		cacheMaxMB = 2048 // 2 GiB
	}

	cacheTTLHours, _ := strconv.Atoi(os.Getenv("CACHE_TTL_HOURS"))
	if cacheTTLHours <= 0 {
		cacheTTLHours = 168 // 7 days
	}

	var err error
	resultCache, err = cache.Open(cacheDB, cache.Options{
		MaxBytes: int64(cacheMaxMB) << 20,
		TTL:      time.Duration(cacheTTLHours) * time.Hour,
	})
	if err != nil {
		slog.Error("open cache db failed", "path", cacheDB, "error", err)
		os.Exit(1)
	}
	defer resultCache.Close()
	slog.Info("cache opened",
		"path", cacheDB,
		"max_mb", cacheMaxMB,
		"ttl_hours", cacheTTLHours,
	)

	// Verify all required OCR models are present before serving. The server
	// always integrates OCR (text conversion, extraction, seal detection), so
	// a missing model is a fatal startup error. Models are still lazily
	// loaded at runtime.
	baseDir, _ := os.Getwd()
	if exe, err := os.Executable(); err == nil {
		baseDir = filepath.Dir(exe)
	}
	if err := ocr.CheckAllModels(baseDir); err != nil {
		slog.Error("required OCR models not found", "error", err)
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Enforce a cap on concurrent OCR-bound conversions (default 4).
	maxConcurrent := maxConcurrentConversions()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /convert/image", limit(maxConcurrent, handleConvertImage))
	mux.HandleFunc("POST /convert/text", limit(maxConcurrent, handleConvertText))
	mux.HandleFunc("POST /convert/doc", limit(maxConcurrent, handleConvertDoc))
	mux.HandleFunc("POST /extract/txt", limit(maxConcurrent, handleExtractTxt))
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("DELETE /cache/clear", handleCacheClear)

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      withLogging(mux),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown on SIGTERM / SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		certFile := os.Getenv("TLS_CERT_FILE")
		keyFile := os.Getenv("TLS_KEY_FILE")
		tlsEnabled := false
		if certFile != "" && keyFile != "" {
			if _, err := os.Stat(certFile); err != nil {
				slog.Warn("TLS_CERT_FILE set but not found, falling back to HTTP", "file", certFile, "error", err)
			} else if _, err := os.Stat(keyFile); err != nil {
				slog.Warn("TLS_KEY_FILE set but not found, falling back to HTTP", "file", keyFile, "error", err)
			} else {
				slog.Info("TLS enabled", "cert", certFile, "key", keyFile)
				tlsEnabled = true
				errCh <- srv.ListenAndServeTLS(certFile, keyFile)
			}
		}
		if !tlsEnabled {
			errCh <- srv.ListenAndServe()
		}
	}()

	slog.Info("server starting", "port", port, "max_concurrent", maxConcurrent)

	select {
	case err := <-errCh:
		if err != http.ErrServerClosed {
			slog.Error("server fatal", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		slog.Info("shutting down gracefully")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("graceful shutdown timed out, forcing close", "error", err)
			srv.Close()
		}
	}
}

func slogLevel() slog.Level {
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func maxConcurrentConversions() int {
	s := os.Getenv("MAX_CONCURRENT_CONVERSIONS")
	if s == "" {
		return 4
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 4
	}
	return n
}

// ── Middleware ────────────────────────────────────────────────────────────────

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// limit returns a handler that rejects requests with 429 Too Many Requests when
// concurrency exceeds capacity.
func limit(n int, next http.HandlerFunc) http.HandlerFunc {
	if n <= 0 {
		// Unbounded (should not happen with current defaults).
		return next
	}
	sema := make(chan struct{}, n)
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case sema <- struct{}{}:
			defer func() { <-sema }()
			next(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"error": "too many concurrent requests"})
		}
	}
}

// ── Handlers ─────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
		slog.Warn("health response write failed", "error", err)
	}
}

func handleCacheClear(w http.ResponseWriter, r *http.Request) {
	before := resultCache.TotalBytes()
	if err := resultCache.Clear(); err != nil {
		slog.Error("cache clear failed", "size_before_bytes", before, "error", err)
		http.Error(w, "cache clear failed", http.StatusInternalServerError)
		return
	}
	slog.Info("cache cleared",
		"size_before_bytes", before,
		"size_after_bytes", resultCache.TotalBytes(),
	)
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"status":"cleared"}`)); err != nil {
		slog.Warn("cache clear response write failed", "error", err)
	}
}

func handleConvertImage(w http.ResponseWriter, r *http.Request) {
	hotwords := doc.ParseHotwords(r.FormValue("hotwords"))
	slog.Info("conversion started with hotwords", "hotwords_count", len(hotwords), "hotwords", hotwords)
	convertAndServe(w, r, doc.ModeImage, hotwords)
}

func handleConvertText(w http.ResponseWriter, r *http.Request) {
	slog.Info("text conversion started")
	convertAndServe(w, r, doc.ModeText, nil)
}

func handleConvertDoc(w http.ResponseWriter, r *http.Request) {
	docTmp, headerFilename, fileHash, err := saveUpload(w, r)
	if err != nil {
		return
	}
	defer os.Remove(docTmp)

	// Validate file type: only OLE2 (.doc) files accepted.
	if !doc.IsOLE2File(docTmp) {
		slog.Warn("unsupported file type for /convert/doc", "filename", headerFilename)
		http.Error(w, "unsupported file type: only .doc (OLE2) files are accepted for /convert/doc", http.StatusBadRequest)
		return
	}

	// Cache hit: return the previously generated .docx without re-running
	// LibreOffice.
	if data, ok := resultCache.Get(cache.BucketDoc, fileHash); ok {
		slog.Info("doc conversion cache hit", "filename", headerFilename, "cache_key", shortKey(fileHash))
		serveDocxBytes(w, data, docxFilename(headerFilename, doc.ModeImage))
		return
	}

	slog.Info("doc conversion started", "filename", headerFilename)

	start := time.Now()
	docxPath, cleanup, err := doc.ConvertOLE2ToDocx(docTmp)
	if err != nil {
		slog.Error("doc conversion failed",
			"filename", headerFilename,
			"error", err,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		http.Error(w, fmt.Sprintf("conversion failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer cleanup()
	defer os.Remove(docxPath)

	slog.Info("doc conversion done",
		"filename", headerFilename,
		"output", filepath.Base(docxPath),
		"duration_ms", time.Since(start).Milliseconds(),
	)

	docxData, err := os.ReadFile(docxPath)
	if err != nil {
		slog.Error("read result failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := resultCache.Put(cache.BucketDoc, fileHash, docxData); err != nil {
		slog.Warn("cache put failed", "bucket", cache.BucketDoc, "error", err)
	}

	serveDocxBytes(w, docxData, docxFilename(headerFilename, doc.ModeImage))
}

func handleExtractTxt(w http.ResponseWriter, r *http.Request) {
	pdfTmp, filename, fileHash, err := saveUpload(w, r)
	if err != nil {
		return
	}
	defer os.Remove(pdfTmp)

	// Cache hit: return the stored JSON without re-extracting.
	if data, ok := resultCache.Get(cache.BucketExtract, fileHash); ok {
		slog.Info("extract text cache hit", "filename", filename, "cache_key", shortKey(fileHash))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if _, err := w.Write(data); err != nil {
			slog.Warn("extract text cache write failed", "error", err)
		}
		return
	}

	slog.Info("extract text started", "filename", filename)

	start := time.Now()
	result, err := pdf.ExtractText(pdfTmp)
	if err != nil {
		slog.Error("extract text failed",
			"filename", filename,
			"error", err,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		http.Error(w, fmt.Sprintf("extract text failed: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("extract text done",
		"filename", filename,
		"text_chars", len([]rune(result.Text)),
		"ocr_confidence", result.OCRConfidence,
		"duration_ms", time.Since(start).Milliseconds(),
	)

	data, err := json.Marshal(result)
	if err != nil {
		slog.Error("marshal extract result failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := resultCache.Put(cache.BucketExtract, fileHash, data); err != nil {
		slog.Warn("cache put failed", "bucket", cache.BucketExtract, "error", err)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if _, err := w.Write(data); err != nil {
		slog.Warn("extract text write failed", "error", err)
	}
}

func convertAndServe(w http.ResponseWriter, r *http.Request, mode doc.ConvertMode, hotwords []string) {
	pdfTmp, headerFilename, fileHash, err := saveUpload(w, r)
	if err != nil {
		return
	}
	defer os.Remove(pdfTmp)

	// Validate file type: /convert/image and /convert/text require PDF.
	if !isPDFFile(pdfTmp) {
		slog.Warn("unsupported file type for conversion", "filename", headerFilename)
		http.Error(w, "unsupported file type: only PDF files are accepted for /convert/image and /convert/text", http.StatusBadRequest)
		return
	}

	bucket, key := convertCacheKey(mode, fileHash, hotwords)

	// Cache hit: return the previously generated .docx without re-converting.
	if data, ok := resultCache.Get(bucket, key); ok {
		slog.Info("conversion cache hit",
			"filename", headerFilename,
			"mode", mode,
			"cache_key", shortKey(key),
		)
		serveDocxBytes(w, data, docxFilename(headerFilename, mode))
		return
	}

	slog.Info("conversion started",
		"filename", headerFilename,
		"mode", mode,
	)

	// Convert.
	convStart := time.Now()
	var docxPath string
	if len(hotwords) > 0 && mode == doc.ModeImage {
		docxPath, err = pdf.ConvertWithHotwords(pdfTmp, mode, hotwords, func(pct float64) {})
	} else {
		docxPath, err = pdf.Convert(pdfTmp, mode, func(pct float64) {})
	}
	convDuration := time.Since(convStart)

	if err != nil {
		slog.Error("conversion failed",
			"filename", headerFilename,
			"error", err,
			"duration_ms", convDuration.Milliseconds(),
			"mode", mode,
		)
		http.Error(w, fmt.Sprintf("conversion failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer os.Remove(docxPath)

	slog.Info("conversion done",
		"filename", headerFilename,
		"output", filepath.Base(docxPath),
		"duration_ms", convDuration.Milliseconds(),
		"mode", mode,
	)

	docxData, err := os.ReadFile(docxPath)
	if err != nil {
		slog.Error("read result failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := resultCache.Put(bucket, key, docxData); err != nil {
		slog.Warn("cache put failed", "bucket", bucket, "error", err)
	}

	serveDocxBytes(w, docxData, docxFilename(headerFilename, mode))
}

// convertCacheKey returns the (bucket, key) pair for a PDF conversion. Image
// mode includes a canonicalised hotword hash so that different hotword sets
// cache independently; text mode has no hotwords so it keys on the file alone.
func convertCacheKey(mode doc.ConvertMode, fileHash []byte, hotwords []string) (string, []byte) {
	if mode == doc.ModeText {
		return cache.BucketText, fileHash
	}

	// Image mode: incorporate the hotwords into the key. Canonicalise (sort +
	// lowercase + de-dupe) so that semantically identical hotword inputs share
	// a cache entry.
	key := make([]byte, len(fileHash)+sha256.Size)
	copy(key, fileHash)
	h := sha256.New()
	if len(hotwords) > 0 {
		canon := make([]string, len(hotwords))
		for i, hw := range hotwords {
			canon[i] = strings.ToLower(hw)
		}
		sort.Strings(canon)
		// De-duplicate adjacent equal entries.
		uniq := canon[:0]
		for i, s := range canon {
			if i == 0 || s != canon[i-1] {
				uniq = append(uniq, s)
			}
		}
		for _, s := range uniq {
			h.Write([]byte(s))
			h.Write([]byte{0})
		}
	}
	sum := h.Sum(nil)
	copy(key[len(fileHash):], sum)
	return cache.BucketImage, key
}

// shortKey returns a short hex identifier for a cache key (first 8 bytes of
// its SHA-256), for human-readable log correlation.
func shortKey(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}

// serveDocxBytes writes a DOCX payload with the appropriate headers.
func serveDocxBytes(w http.ResponseWriter, data []byte, filename string) {
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	if _, err := w.Write(data); err != nil {
		slog.Warn("stream response interrupted", "error", err)
	}
}

// saveUpload reads a multipart file upload, writes it to a temp file, and
// computes the file's SHA-256 content hash for cache keying.
// On success it returns the temp file path, original filename, and hash.
// On error it writes the HTTP error and returns a non-nil err.
func saveUpload(w http.ResponseWriter, r *http.Request) (tmpPath, filename string, fileHash []byte, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		slog.Warn("parse multipart failed", "error", err)
		http.Error(w, fmt.Sprintf("failed to parse form: %v", err), http.StatusBadRequest)
		return "", "", nil, err
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("file")
	if err != nil {
		slog.Warn("missing file field", "error", err)
		http.Error(w, "missing file field", http.StatusBadRequest)
		return "", "", nil, err
	}
	defer file.Close()

	// Preserve the original extension for correct MIME/magic detection.
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".tmp"
	}
	tmp, err := os.CreateTemp("", "docutil-*"+ext)
	if err != nil {
		slog.Error("create temp file failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", "", nil, err
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), file); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		slog.Error("write temp file failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", "", nil, err
	}
	tmp.Close()

	slog.Info("upload saved",
		"filename", header.Filename,
		"size_bytes", header.Size,
		"sha256", hex.EncodeToString(hasher.Sum(nil)),
	)
	return tmp.Name(), header.Filename, hasher.Sum(nil), nil
}

// isPDFFile checks whether the file at path is a PDF by magic bytes.
func isPDFFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var magic [4]byte
	n, _ := io.ReadFull(f, magic[:])
	if n < 4 {
		return false
	}
	// "%PDF"
	return magic[0] == '%' && magic[1] == 'P' && magic[2] == 'D' && magic[3] == 'F'
}

func docxFilename(pdfName string, mode doc.ConvertMode) string {
	ext := filepath.Ext(pdfName)
	base := pdfName[:len(pdfName)-len(ext)]
	if mode == doc.ModeText {
		return base + ".text.docx"
	}
	return base + ".docx"
}
