package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/genai"
)

// --- CONFIGURATION ---
const Version = "v1.07"

var (
	// Configurable paths via flags
	watchDir string
	destDir  string

	// Other configurations
	modelName        string
	concurrencyLimit int

	// File stability timings (variables to allow fast overrides in tests)
	stabilityThreshold     = 10 * time.Second
	stabilityPollWait      = 1 * time.Second
	maxStabilityWait       = 5 * time.Minute
	activeFileCleanupDelay = 2 * time.Second
)

// Global tracker to prevent double-processing
var activeFiles sync.Map

func main() {
	// 0. Parse Flags
	flag.StringVar(&watchDir, "watch", "", "Directory to watch for new receipts (required)")
	flag.StringVar(&destDir, "dest", "", "Directory to save processed receipts (required)")
	flag.StringVar(&modelName, "model", "gemini-flash-latest", "Gemini model to use")
	flag.IntVar(&concurrencyLimit, "workers", 2, "Number of concurrent processing workers")
	flag.Parse()

	// Disable standard Go log timestamp prefix since service supervisors (runit/vlogger/systemd) add their own timestamps
	log.SetFlags(0)
	log.SetOutput(os.Stdout)

	if watchDir == "" || destDir == "" {
		flag.Usage()
		log.Fatal("Both -watch and -dest flags are required")
	}
	if concurrencyLimit < 1 {
		concurrencyLimit = 1
	}

	log.Printf("Starting scanner-bot %s", Version)

	// 1. Setup Gemini Client
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		log.Fatal("GEMINI_API_KEY environment variable not set")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Fatal(err)
	}

	// 2. Setup File Watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	semaphore := make(chan struct{}, concurrencyLimit)
	var wg sync.WaitGroup

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				// Trigger on file creation or modification (exclude Rename/Remove which fire when archiving out of watchDir)
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Chmod) {
					enqueueFile(ctx, client, semaphore, &wg, event.Name)
				} else if !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
					log.Printf("Ignored event: %v", event)
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("Watcher error:", err)
			}
		}
	}()

	if err := watcher.Add(watchDir); err != nil {
		log.Fatalf("Failed to watch directory %s: %v", watchDir, err)
	}
	log.Printf("Listening for receipts in %s...", watchDir)
	log.Printf("Saving processed files to %s...", destDir)

	// Sweep existing files already present in watchDir at startup
	scanExistingFiles(ctx, client, semaphore, &wg, watchDir)

	<-ctx.Done()
	log.Println("Shutting down... waiting for pending tasks to finish")
	wg.Wait()
	log.Println("Shutdown complete.")
}

// isSupportedReceiptFile checks whether path is a non-hidden supported image/PDF file
func isSupportedReceiptFile(path string) bool {
	base := filepath.Base(path)
	if base == "" || strings.HasPrefix(base, ".") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(base))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".pdf"
}

// scanExistingFiles enqueues any supported files already present in dir on startup
func scanExistingFiles(ctx context.Context, client *genai.Client, semaphore chan struct{}, wg *sync.WaitGroup, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("Failed to read watch directory %s on startup: %v", dir, err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		enqueueFile(ctx, client, semaphore, wg, filepath.Join(dir, entry.Name()))
	}
}

// enqueueFile filters and deduplicates a file path before dispatching a worker goroutine
func enqueueFile(ctx context.Context, client *genai.Client, semaphore chan struct{}, wg *sync.WaitGroup, path string) {
	path = filepath.Clean(path)
	if !isSupportedReceiptFile(path) {
		return
	}
	// Verify file still exists before enqueuing (guards against trailing events after archive/removal)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return
	}
	// DEDUPLICATION: Check if we are already handling this file
	if _, loaded := activeFiles.LoadOrStore(path, true); loaded {
		return
	}

	wg.Add(1)
	go func(p string) {
		defer wg.Done()
		defer func() {
			if activeFileCleanupDelay > 0 {
				time.AfterFunc(activeFileCleanupDelay, func() {
					activeFiles.Delete(p)
				})
			} else {
				activeFiles.Delete(p)
			}
		}()

		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
		case <-ctx.Done():
			return
		}

		processEvent(ctx, client, p)
	}(path)
}

func processEvent(ctx context.Context, client *genai.Client, path string) {
	log.Printf("Detected: %s. Waiting for write to complete...", path)

	if err := waitForStableFile(ctx, path); err != nil {
		log.Printf("Processing aborted for %s: %v", path, err)
		return
	}

	var sizeBytes int64
	if info, err := os.Stat(path); err == nil {
		sizeBytes = info.Size()
	}
	log.Printf("Processing: %s (size=%d bytes)", path, sizeBytes)

	dataList, err := analyzeReceipt(ctx, client, path)
	if err != nil {
		log.Printf("Analysis failed for %s: %v", path, err)
		return
	}

	if len(dataList) == 0 {
		log.Printf("No receipt data found in %s", path)
		return
	}

	saveAndArchive(path, dataList)
}

// waitForStableFile monitors the file until size is constant for a duration
func waitForStableFile(ctx context.Context, path string) error {
	startTime := time.Now()
	lastSize := int64(-1)
	stableSince := time.Now()

	ticker := time.NewTicker(stabilityPollWait)
	defer ticker.Stop()

	for {
		if time.Since(startTime) > maxStabilityWait {
			return fmt.Errorf("timeout waiting for file to stabilize")
		}

		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return fmt.Errorf("file disappeared")
		}
		if err != nil {
			return fmt.Errorf("error stating file: %w", err)
		}

		currentSize := info.Size()

		if currentSize != lastSize {
			lastSize = currentSize
			stableSince = time.Now()
		} else if time.Since(stableSince) >= stabilityThreshold && currentSize > 0 {
			return nil // Stable
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
