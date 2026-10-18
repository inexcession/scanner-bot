package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/genai"
)

// --- CONFIGURATION ---
const Version = "v1.02"

var (
	// Configurable paths via flags
	watchDir string
	destDir  string

	// Other configurations
	modelName        string
	useSyslog        bool
	concurrencyLimit int

	// File stability timings (variables to allow fast overrides in tests)
	stabilityThreshold = 10 * time.Second
	stabilityPollWait  = 1 * time.Second
	maxStabilityWait   = 5 * time.Minute
)

// ReceiptData maps the JSON response from Gemini
type ReceiptData struct {
	Date     string `json:"date"`
	Vendor   string `json:"vendor"`
	Category string `json:"category"`
	Amount   int    `json:"total_amount"`
}

// Allowed receipt categories
var validCategories = map[string]string{
	"medical":   "Medical",
	"grocery":   "Grocery",
	"tax":       "Tax",
	"utilities": "Utilities",
	"septic":    "Septic",
	"other":     "Other",
	"unsorted":  "Unsorted",
}

// Structured output schema for Gemini receipt extraction
var receiptResponseSchema = &genai.Schema{
	Type:        genai.TypeArray,
	Description: "List of receipts or certificates found in the document",
	Items: &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"date": {
				Type:        genai.TypeString,
				Description: "Receipt date in YYYY-MM-DD format",
			},
			"vendor": {
				Type:        genai.TypeString,
				Description: "Japanese vendor name; if medical, use clinic name",
			},
			"category": {
				Type:        genai.TypeString,
				Format:      "enum",
				Enum:        []string{"Medical", "Grocery", "Tax", "Utilities", "Septic", "Other"},
				Description: "Expense category",
			},
			"total_amount": {
				Type:        genai.TypeInteger,
				Description: "Total amount in JPY as an integer",
			},
		},
		Required: []string{"date", "vendor", "category", "total_amount"},
	},
}

// Global tracker to prevent double-processing
var activeFiles sync.Map

func main() {
	// 0. Parse Flags
	flag.StringVar(&watchDir, "watch", "", "Directory to watch for new receipts (required)")
	flag.StringVar(&destDir, "dest", "", "Directory to save processed receipts (required)")
	flag.StringVar(&modelName, "model", "gemini-flash-latest", "Gemini model to use")
	flag.BoolVar(&useSyslog, "syslog", true, "Enable syslog output")
	flag.IntVar(&concurrencyLimit, "workers", 2, "Number of concurrent processing workers")
	flag.Parse()

	if useSyslog {
		syslogger, err := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "scanner-bot")
		if err == nil {
			log.SetOutput(syslogger)
			log.SetFlags(0)
		} else {
			log.Printf("Failed to connect to syslog: %v", err)
		}
	}

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

				// Trigger on any modification that might indicate a file is ready
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) || event.Has(fsnotify.Chmod) {
					enqueueFile(ctx, client, semaphore, &wg, event.Name)
				} else {
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
	if !isSupportedReceiptFile(path) {
		return
	}
	// DEDUPLICATION: Check if we are already handling this file
	if _, loaded := activeFiles.LoadOrStore(path, true); loaded {
		return
	}

	wg.Add(1)
	go func(p string) {
		defer wg.Done()
		defer activeFiles.Delete(p)

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

	log.Printf("Processing: %s", path)

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

// analyzeReceipt uploads the file to Gemini and extracts receipt data
func analyzeReceipt(ctx context.Context, client *genai.Client, path string) ([]ReceiptData, error) {
	// Upload
	upFile, err := client.Files.UploadFromPath(ctx, path, nil)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	defer func(fileName string) {
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := client.Files.Delete(delCtx, fileName, nil); err != nil {
			log.Printf("Failed to delete uploaded file %s: %v", fileName, err)
		}
	}(upFile.Name)

	// Wait for processing with a bounded timeout
	pollCtx, pollCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer pollCancel()

	for upFile.State == genai.FileStateProcessing {
		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf("interrupted waiting for remote file processing: %w", pollCtx.Err())
		case <-time.After(1 * time.Second):
		}

		upFile, err = client.Files.Get(pollCtx, upFile.Name, nil)
		if err != nil {
			return nil, fmt.Errorf("check failed state: %w", err)
		}
	}

	if upFile.State != genai.FileStateActive {
		return nil, fmt.Errorf("file processing failed state: %s", upFile.State)
	}

	// Generate
	prompt := `Analyze this Japanese receipt or certificate. Extract JSON with these keys:
    "date" (YYYY-MM-DD),
    "vendor" (Japanese name, if medical use clinic name),
    "category" (Medical, Grocery, Tax, Utilities, Septic, Other),
    "total_amount" (integer).`

	contents := []*genai.Content{
		genai.NewContentFromParts([]*genai.Part{
			genai.NewPartFromFile(*upFile),
			genai.NewPartFromText(prompt),
		}, genai.RoleUser),
	}

	resp, err := client.Models.GenerateContent(ctx, modelName, contents, &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
		ResponseSchema:   receiptResponseSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini generate error: %w", err)
	}

	jsonText := resp.Text()
	if jsonText == "" {
		return nil, fmt.Errorf("empty response from model")
	}

	return parseGeminiResponse(jsonText)
}

func parseGeminiResponse(jsonText string) ([]ReceiptData, error) {
	// Robustly extract JSON by finding the first and last structural characters
	firstBrace := strings.IndexAny(jsonText, "{[")
	lastBrace := strings.LastIndexAny(jsonText, "}]")

	if firstBrace != -1 && lastBrace != -1 && lastBrace >= firstBrace {
		jsonText = jsonText[firstBrace : lastBrace+1]
	}

	var dataList []ReceiptData
	var single ReceiptData

	// Attempt 1: Single Object
	if err := json.Unmarshal([]byte(jsonText), &single); err == nil {
		dataList = append(dataList, single)
		return dataList, nil
	}

	// Attempt 2: Array of Objects
	var list []ReceiptData
	if err := json.Unmarshal([]byte(jsonText), &list); err == nil {
		return list, nil
	}

	return nil, fmt.Errorf("failed to parse JSON as object or array")
}

func saveAndArchive(srcPath string, dataList []ReceiptData) {
	successCount := 0
	for _, data := range dataList {
		if err := saveProcessedFile(srcPath, data); err != nil {
			log.Printf("Failed to save processed file: %v", err)
		} else {
			successCount++
		}
	}

	if successCount > 0 {
		archiveOriginalFile(srcPath)
	} else {
		log.Printf("No receipts saved, skipping archive for %s", srcPath)
	}
}

// sanitizeCategory validates category against the allowlist and defaults to "Unsorted"
func sanitizeCategory(cat string) string {
	trimmed := strings.ToLower(strings.TrimSpace(cat))
	if canonical, ok := validCategories[trimmed]; ok {
		return canonical
	}
	return "Unsorted"
}

// sanitizeVendor removes whitespace, path separators, and control characters from a vendor name
func sanitizeVendor(vendor string) string {
	var b strings.Builder
	for _, r := range vendor {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			continue
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	res := strings.Trim(b.String(), ".-")
	if res == "" {
		return "Unknown"
	}
	return res
}

// sanitizeDate ensures the date is in YYYY-MM-DD format, falling back to today's date
func sanitizeDate(dateStr string) string {
	trimmed := strings.TrimSpace(dateStr)
	if _, err := time.Parse("2006-01-02", trimmed); err == nil {
		return trimmed
	}
	return time.Now().Format("2006-01-02")
}

// uniquePath appends _1, _2, etc. before the file extension if the target path already exists
func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func saveProcessedFile(srcPath string, data ReceiptData) error {
	vendor := sanitizeVendor(data.Vendor)
	date := sanitizeDate(data.Date)
	category := sanitizeCategory(data.Category)

	processedFileName := fmt.Sprintf("%s_%s_%d円%s", date, vendor, data.Amount, filepath.Ext(srcPath))
	processedDir := filepath.Join(destDir, category)

	if err := os.MkdirAll(processedDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", processedDir, err)
	}

	processedPath := uniquePath(filepath.Join(processedDir, processedFileName))

	if err := robustCopy(srcPath, processedPath); err != nil {
		return fmt.Errorf("failed to copy to processed folder: %w", err)
	}

	log.Printf("Saved processed file: %s", processedPath)
	return nil
}

func archiveOriginalFile(srcPath string) {
	originalsDir := filepath.Join(destDir, "originals")
	originalName := filepath.Base(srcPath)

	if err := os.MkdirAll(originalsDir, 0755); err != nil {
		log.Printf("Failed to create originals directory: %v", err)
		return
	}

	originalsPath := uniquePath(filepath.Join(originalsDir, originalName))

	if err := robustMove(srcPath, originalsPath); err != nil {
		log.Printf("Failed to move to originals: %v", err)
	} else {
		log.Printf("Archived original to: %s", originalsPath)
	}
}

// robustCopy performs a copy of the file content and checks close/flush errors
func robustCopy(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(destFile, sourceFile)
	closeErr := destFile.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	return nil
}

// robustMove tries atomic rename first, then falls back to Copy+Delete
func robustMove(src, dst string) error {
	// Try atomic rename
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}

	// If error is NOT cross-device, fail
	if !errors.Is(err, syscall.EXDEV) && !strings.Contains(err.Error(), "cross-device link") {
		return err
	}

	// Fallback: Copy and Delete
	if err := robustCopy(src, dst); err != nil {
		return err
	}

	return os.Remove(src)
}
