package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

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

// ReceiptData maps the JSON response from Gemini
type ReceiptData struct {
	RawDate            string `json:"raw_date"`
	Date               string `json:"date"`
	Vendor             string `json:"vendor"`
	Category           string `json:"category"`
	Amount             int    `json:"total_amount"`
	InsuredCopayAmount int    `json:"insured_copay_amount,omitempty"`
	UninsuredAmount    int    `json:"uninsured_amount,omitempty"`
	PatientName        string `json:"patient_name,omitempty"`
	IsTreatment        bool   `json:"is_treatment,omitempty"`
	IsMedicine         bool   `json:"is_medicine,omitempty"`
	IsNursingCare      bool   `json:"is_nursing_care,omitempty"`
	IsOtherMedical     bool   `json:"is_other_medical,omitempty"`
	ReimbursedAmount   int    `json:"reimbursed_amount,omitempty"`
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
			"raw_date": {
				Type:        genai.TypeString,
				Description: "Exact date text as printed on the receipt (e.g. '令和8年10月18日', '令8.10.18', 'R8/10/18', '2026年10月18日')",
			},
			"date": {
				Type:        genai.TypeString,
				Description: "Receipt date converted to YYYY-MM-DD format (Reiwa N = 2018 + N, so Reiwa 8 / 令和8年 = 2026)",
			},
			"vendor": {
				Type:        genai.TypeString,
				Description: "Japanese vendor name; if medical, use clinic or pharmacy name",
			},
			"category": {
				Type:        genai.TypeString,
				Format:      "enum",
				Enum:        []string{"Medical", "Grocery", "Tax", "Utilities", "Septic", "Other"},
				Description: "Expense category",
			},
			"total_amount": {
				Type:        genai.TypeInteger,
				Description: "Total receipt amount paid in JPY (領収額合計) as an integer",
			},
			"insured_copay_amount": {
				Type:        genai.TypeInteger,
				Description: "For Medical receipts: health-insurance-covered patient copay amount in JPY (保険負担額) if broken down on the receipt; otherwise equal to total_amount (or 0 if non-medical).",
			},
			"uninsured_amount": {
				Type:        genai.TypeInteger,
				Description: "For Medical receipts: out-of-insurance / self-pay amount in JPY (保険外負担合計) if broken down on the receipt; otherwise 0.",
			},
			"patient_name": {
				Type:        genai.TypeString,
				Description: "For Medical receipts: patient full name (医療を受けた人) as printed on the receipt without honorifics like 様. Empty string if not medical or not printed.",
			},
			"is_treatment": {
				Type:        genai.TypeBoolean,
				Description: "For Medical receipts (NTA 医療費の区分 - 診療・治療): true if hospital, clinic, or dental consultation/treatment.",
			},
			"is_medicine": {
				Type:        genai.TypeBoolean,
				Description: "For Medical receipts (NTA 医療費の区分 - 医薬品購入): true if prescription pharmacy (調剤薬局) or drugstore medicine purchase.",
			},
			"is_nursing_care": {
				Type:        genai.TypeBoolean,
				Description: "For Medical receipts (NTA 医療費の区分 - 介護保険サービス): true if nursing care insurance service.",
			},
			"is_other_medical": {
				Type:        genai.TypeBoolean,
				Description: "For Medical receipts (NTA 医療費の区分 - その他の医療費): true if other qualifying medical expense (e.g. medical equipment, transportation).",
			},
			"reimbursed_amount": {
				Type:        genai.TypeInteger,
				Description: "For Medical receipts (NTA 左のうち、補填される金額): insurance or municipal reimbursement amount in JPY if explicitly stated on the receipt, otherwise 0.",
			},
		},
		PropertyOrdering: []string{
			"raw_date", "date", "vendor", "category", "total_amount",
			"insured_copay_amount", "uninsured_amount",
			"patient_name", "is_treatment", "is_medicine", "is_nursing_care", "is_other_medical", "reimbursed_amount",
		},
		Required: []string{
			"raw_date", "date", "vendor", "category", "total_amount",
			"insured_copay_amount", "uninsured_amount",
			"patient_name", "is_treatment", "is_medicine", "is_nursing_care", "is_other_medical", "reimbursed_amount",
		},
	},
}

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

// formatUsageStats formats Gemini response metadata (model version, latency, token counts) for logging
func formatUsageStats(resp *genai.GenerateContentResponse, elapsed time.Duration) string {
	modelVer := modelName
	if resp != nil && resp.ModelVersion != "" {
		modelVer = resp.ModelVersion
	}
	if resp == nil || resp.UsageMetadata == nil {
		return fmt.Sprintf("model=%s, latency=%s", modelVer, elapsed.Round(time.Millisecond))
	}
	u := resp.UsageMetadata
	if u.ThoughtsTokenCount > 0 {
		return fmt.Sprintf("model=%s, latency=%s, tokens: prompt=%d candidates=%d thoughts=%d total=%d",
			modelVer, elapsed.Round(time.Millisecond),
			u.PromptTokenCount, u.CandidatesTokenCount, u.ThoughtsTokenCount, u.TotalTokenCount)
	}
	return fmt.Sprintf("model=%s, latency=%s, tokens: prompt=%d candidates=%d total=%d",
		modelVer, elapsed.Round(time.Millisecond),
		u.PromptTokenCount, u.CandidatesTokenCount, u.TotalTokenCount)
}

// analyzeReceipt uploads the file to Gemini and extracts receipt data
func analyzeReceipt(ctx context.Context, client *genai.Client, path string) ([]ReceiptData, error) {
	baseName := filepath.Base(path)

	// 1. Upload
	uploadStart := time.Now()
	log.Printf("Uploading to Gemini Files API: %s", baseName)
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

	log.Printf("Uploaded %s -> %s (mime=%s, state=%s, took=%s)",
		baseName, upFile.Name, upFile.MIMEType, upFile.State, time.Since(uploadStart).Round(time.Millisecond))

	// 2. Wait for remote file processing with a bounded timeout
	pollCtx, pollCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer pollCancel()

	if upFile.State == genai.FileStateProcessing {
		log.Printf("Waiting for Gemini remote file processing: %s (%s)...", baseName, upFile.Name)
	}
	pollStart := time.Now()
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
	if time.Since(pollStart) >= time.Second {
		log.Printf("Remote file ready: %s (took=%s)", upFile.Name, time.Since(pollStart).Round(time.Millisecond))
	}

	// 3. Generate
	now := time.Now()
	currentYear := now.Year()
	reiwaYear := currentYear - 2018

	prompt := fmt.Sprintf(`Analyze this Japanese receipt or certificate.
Today's date is %s (Year %d = Reiwa %d / 令和%d年).

Extract JSON with these keys:
    "raw_date" (exact date string printed on the receipt, e.g. "令和%d年10月18日", "令%d.10.18", "R%d/10/18"),
    "date" (YYYY-MM-DD),
    "vendor" (Japanese name, if medical use clinic or pharmacy name),
    "category" (Medical, Grocery, Tax, Utilities, Septic, Other),
    "total_amount" (integer total receipt amount paid 領収額合計 in JPY),
    "insured_copay_amount" (integer, for Medical: health-insurance-covered patient copay 保険負担額 in JPY if listed separately; otherwise equal to total_amount),
    "uninsured_amount" (integer, for Medical: out-of-insurance amount 保険外負担合計 in JPY if listed separately; otherwise 0),
    "patient_name" (for Medical: patient full name 医療を受けた人 printed on the receipt, without honorifics like 様; empty string if not printed or not Medical),
    "is_treatment" (boolean, NTA 診療・治療: true for hospital/clinic/dental treatment),
    "is_medicine" (boolean, NTA 医薬品購入: true for dispensing pharmacy 調剤薬局 or drugstore medicine purchase),
    "is_nursing_care" (boolean, NTA 介護保険サービス: true for nursing care insurance service),
    "is_other_medical" (boolean, NTA その他の医療費: true for other qualifying medical expenses),
    "reimbursed_amount" (integer, NTA 左のうち、補填される金額: insurance/municipal reimbursement amount if explicitly stated on receipt, else 0).

Japanese Era (Wareki) Conversion Rules:
- Reiwa (令和 / 令 / R): Year = 2018 + N (Reiwa 1/元年 = 2019, Reiwa 6 = 2024, Reiwa 7 = 2025, Reiwa 8 / 令和8年 / 令8 / R8 = 2026).
- Heisei (平成 / 平 / H): Year = 1988 + N.
- Read the era year digit carefully on medical receipts (do not confuse 令和%d年 with 令和5年 or 令和6年).
- If the document contains multiple pages for the same receipt/transaction, return only a single entry for that transaction.`,
		now.Format("2006-01-02"), currentYear, reiwaYear, reiwaYear, reiwaYear, reiwaYear, reiwaYear, reiwaYear)

	contents := []*genai.Content{
		genai.NewContentFromParts([]*genai.Part{
			genai.NewPartFromFile(*upFile),
			genai.NewPartFromText(prompt),
		}, genai.RoleUser),
	}

	llmStart := time.Now()
	log.Printf("Sent to LLM (%s), waiting for response: %s...", modelName, baseName)
	resp, err := client.Models.GenerateContent(ctx, modelName, contents, &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
		ResponseSchema:   receiptResponseSchema,
	})
	llmElapsed := time.Since(llmStart)
	if err != nil {
		return nil, fmt.Errorf("gemini generate error (after %s): %w", llmElapsed.Round(time.Millisecond), err)
	}

	log.Printf("LLM response received for %s (%s)", baseName, formatUsageStats(resp, llmElapsed))

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

// deduplicateReceipts removes duplicate receipt entries that normalize to the same date, vendor, category, and amount
func deduplicateReceipts(dataList []ReceiptData) []ReceiptData {
	type receiptKey struct {
		date     string
		vendor   string
		category string
		amount   int
	}
	seen := make(map[receiptKey]struct{}, len(dataList))
	result := make([]ReceiptData, 0, len(dataList))
	for _, d := range dataList {
		key := receiptKey{
			date:     resolveReceiptDate(d),
			vendor:   sanitizeVendor(d.Vendor),
			category: sanitizeCategory(d.Category),
			amount:   d.Amount,
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, d)
	}
	return result
}

func saveAndArchive(srcPath string, dataList []ReceiptData) {
	uniqueList := deduplicateReceipts(dataList)
	successCount := 0
	for _, data := range uniqueList {
		resolvedDate := resolveReceiptDate(data)
		dateInfo := fmt.Sprintf("raw_date=%q -> date=%s", data.RawDate, resolvedDate)
		if data.RawDate != "" && data.Date != "" && resolvedDate != data.Date {
			dateInfo = fmt.Sprintf("raw_date=%q (llm_date=%q -> resolved=%s)", data.RawDate, data.Date, resolvedDate)
		}
		if sanitizeCategory(data.Category) == "Medical" {
			log.Printf("Extracted receipt: %s, patient=%q, vendor=%q, category=%q, amount=%d円 (insured=%d円, uninsured=%d円, treatment=%t, medicine=%t, nursing=%t, other=%t, reimbursed=%d円)",
				dateInfo, sanitizePatientName(data.PatientName), data.Vendor, data.Category, data.Amount,
				data.InsuredCopayAmount, data.UninsuredAmount,
				data.IsTreatment, data.IsMedicine, data.IsNursingCare, data.IsOtherMedical, data.ReimbursedAmount)
		} else {
			log.Printf("Extracted receipt: %s, vendor=%q, category=%q, amount=%d円",
				dateInfo, data.Vendor, data.Category, data.Amount)
		}
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

// sanitizePatientName cleans control characters and trailing honorifics (e.g. 様) from a patient name
func sanitizePatientName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	cleaned = strings.TrimSpace(cleaned)
	for _, suffix := range []string{"様", "殿", "さん"} {
		cleaned = strings.TrimSpace(strings.TrimSuffix(cleaned, suffix))
	}
	return cleaned
}

// toFullWidth converts ASCII spaces, digits, and letters to full-width (全角) for NTA Excel form columns
func toFullWidth(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastWasSpace := false
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsSpace(r) {
			if !lastWasSpace {
				b.WriteRune('　') // U+3000 Ideographic Space
				lastWasSpace = true
			}
			continue
		}
		lastWasSpace = false
		switch {
		case r >= '!' && r <= '~':
			b.WriteRune(r + 0xFEE0)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// truncateRunes caps a string to at most maxRunes characters
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}

// formatNTAPatient formats a patient name for NTA Column B (全角10文字以内)
func formatNTAPatient(name string) string {
	fw := toFullWidth(sanitizePatientName(name))
	if len([]rune(fw)) > 10 {
		// Try removing full-width space if name is slightly over 10 chars
		compact := strings.ReplaceAll(fw, "　", "")
		if len([]rune(compact)) <= 10 {
			return compact
		}
		return truncateRunes(compact, 10)
	}
	return fw
}

// formatNTAProvider formats a hospital/pharmacy name for NTA Column C (全角20文字以内)
func formatNTAProvider(provider string) string {
	fw := toFullWidth(provider)
	if len([]rune(fw)) <= 20 {
		return fw
	}
	// Strip common corporate prefixes when exceeding 20 characters (matching typical NTA form practice)
	for _, prefix := range []string{
		"独立行政法人国立病院機構",
		"地方独立行政法人",
		"社会医療法人社団",
		"社会医療法人財団",
		"社会医療法人",
		"医療法人社団",
		"医療法人財団",
		"医療法人",
		"株式会社",
		"有限会社",
	} {
		if strings.HasPrefix(fw, prefix) {
			trimmed := strings.TrimPrefix(fw, prefix)
			trimmed = strings.TrimLeft(trimmed, "　")
			if trimmed != "" {
				fw = trimmed
				break
			}
		}
	}
	if len([]rune(fw)) > 20 {
		compact := strings.ReplaceAll(fw, "　", "")
		if len([]rune(compact)) <= 20 {
			return compact
		}
		return truncateRunes(compact, 20)
	}
	return fw
}

var (
	// Matches Japanese era dates such as 令和8年10月18日, 令8.10.18, R8/10/18, 令和元年5月1日, 平成31年4月30日
	warekiRegex = regexp.MustCompile(`(?i)(令和|令|R|平成|平|H|昭和|昭|S)\s*(元|\d{1,2})\s*(?:年|[./\-])\s*(\d{1,2})\s*(?:月|[./\-])\s*(\d{1,2})\s*日?`)
	// Matches Gregorian dates such as 2026-10-18, 2026/10/18, 2026.10.18, 2026年10月18日
	gregorianRegex = regexp.MustCompile(`(\d{4})\s*(?:年|[./\-])\s*(\d{1,2})\s*(?:月|[./\-])\s*(\d{1,2})\s*日?`)
	// Matches compact 2-digit year or Reiwa year dates such as 26.10.18 or 8.10.18 when preceded/used as full date string
	shortYearDateRegex = regexp.MustCompile(`^\s*(\d{1,2})\s*(?:年|[./\-])\s*(\d{1,2})\s*(?:月|[./\-])\s*(\d{1,2})\s*日?\s*$`)
)

// normalizeFullWidth converts full-width ASCII digits and punctuation (e.g. ０-９, ／, ．) to standard ASCII
func normalizeFullWidth(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= '０' && r <= '９':
			b.WriteRune('0' + (r - '０'))
		case r >= 'Ａ' && r <= 'Ｚ':
			b.WriteRune('A' + (r - 'Ａ'))
		case r >= 'ａ' && r <= 'ｚ':
			b.WriteRune('a' + (r - 'ａ'))
		case r == '／':
			b.WriteRune('/')
		case r == '．' || r == '。':
			b.WriteRune('.')
		case r == '－' || r == 'ー' || r == '―':
			b.WriteRune('-')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseJapaneseDate attempts to parse Japanese era (Wareki) or Gregorian date strings into YYYY-MM-DD
func parseJapaneseDate(raw string) (string, bool) {
	norm := strings.TrimSpace(normalizeFullWidth(raw))
	if norm == "" {
		return "", false
	}

	// 1. Check explicit Japanese era (令和 / 令 / R, 平成 / 平 / H, 昭和 / 昭 / S)
	if m := warekiRegex.FindStringSubmatch(norm); m != nil {
		era := strings.ToUpper(m[1])
		eraYear := 1
		if m[2] != "元" {
			y, err := strconv.Atoi(m[2])
			if err != nil || y < 1 {
				return "", false
			}
			eraYear = y
		}
		month, _ := strconv.Atoi(m[3])
		day, _ := strconv.Atoi(m[4])

		var baseYear int
		switch era {
		case "令和", "令", "R":
			baseYear = 2018
		case "平成", "平", "H":
			baseYear = 1988
		case "昭和", "昭", "S":
			baseYear = 1925
		default:
			return "", false
		}

		candidate := fmt.Sprintf("%04d-%02d-%02d", baseYear+eraYear, month, day)
		if _, err := time.Parse("2006-01-02", candidate); err == nil {
			return candidate, true
		}
	}

	// 2. Check 4-digit Gregorian date (YYYY-MM-DD, YYYY/MM/DD, YYYY.MM.DD, YYYY年M月D日)
	if m := gregorianRegex.FindStringSubmatch(norm); m != nil {
		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		candidate := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
		if _, err := time.Parse("2006-01-02", candidate); err == nil {
			return candidate, true
		}
	}

	// 3. Check bare short-year date (e.g. "8.10.18" -> Reiwa 8 = 2026, or "26.10.18" -> 2026)
	if m := shortYearDateRegex.FindStringSubmatch(norm); m != nil {
		y, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		var year int
		if y >= 1 && y <= 15 {
			year = 2018 + y // Reiwa year
		} else if y >= 20 && y <= 99 {
			year = 2000 + y // 2-digit Gregorian year
		}
		if year > 0 {
			candidate := fmt.Sprintf("%04d-%02d-%02d", year, month, day)
			if _, err := time.Parse("2006-01-02", candidate); err == nil {
				return candidate, true
			}
		}
	}

	return "", false
}

// resolveReceiptDate prefers deterministic parsing of RawDate (especially Japanese era dates) before falling back to Date
func resolveReceiptDate(data ReceiptData) string {
	if parsed, ok := parseJapaneseDate(data.RawDate); ok {
		return parsed
	}
	return sanitizeDate(data.Date)
}

// sanitizeDate ensures the date is in YYYY-MM-DD format, supporting Japanese eras and falling back to today's date
func sanitizeDate(dateStr string) string {
	if parsed, ok := parseJapaneseDate(dateStr); ok {
		return parsed
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

// yamlString safely formats a scalar string for YAML, quoting only when necessary
func yamlString(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return `""`
	}
	needsQuote := false
	for i, r := range s {
		if unicode.IsControl(r) {
			needsQuote = true
			break
		}
		switch r {
		case ':', '#', '"', '\'', '{', '}', '[', ']', ',', '&', '*', '?', '|', '>', '%', '@', '`', '!':
			needsQuote = true
		}
		if i == 0 && (r == '-' || r == ' ') {
			needsQuote = true
		}
	}
	if needsQuote {
		escaped := strings.ReplaceAll(s, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		return `"` + escaped + `"`
	}
	return s
}

// ntaFlag converts a boolean into the NTA 医療費集計フォーム dropdown value ("該当する" or "")
func ntaFlag(b bool) string {
	if b {
		return "該当する"
	}
	return ""
}

// formatMedicalYAML serializes ReceiptData and NTA 医療費集計フォーム Ver.3.1 fields as clean YAML
func formatMedicalYAML(data ReceiptData, resolvedDate string) string {
	patient := sanitizePatientName(data.PatientName)
	provider := strings.TrimSpace(data.Vendor)
	if provider == "" {
		provider = sanitizeVendor(data.Vendor)
	}

	isTreatment := data.IsTreatment
	isMedicine := data.IsMedicine
	isNursing := data.IsNursingCare
	isOther := data.IsOtherMedical

	// Default to treatment or medicine if Gemini left all 4 false on a Medical receipt
	if !isTreatment && !isMedicine && !isNursing && !isOther {
		if strings.Contains(provider, "薬局") || strings.Contains(provider, "ドラッグ") || strings.Contains(provider, "ファーマシー") {
			isMedicine = true
		} else {
			isTreatment = true
		}
	}

	insuredCopay := data.InsuredCopayAmount
	uninsured := data.UninsuredAmount
	if insuredCopay == 0 && uninsured == 0 && data.Amount > 0 {
		insuredCopay = data.Amount
	}

	reimbursed := data.ReimbursedAmount
	if reimbursed < 0 {
		reimbursed = 0
	}
	reimbursedCol := `""`
	if reimbursed > 0 {
		reimbursedCol = strconv.Itoa(reimbursed)
	}

	slashDate := strings.ReplaceAll(resolvedDate, "-", "/")
	ntaPatient := formatNTAPatient(patient)
	ntaProvider := formatNTAProvider(provider)

	var b strings.Builder
	fmt.Fprintf(&b, "date: %s\n", yamlString(resolvedDate))
	fmt.Fprintf(&b, "raw_date: %s\n", yamlString(data.RawDate))
	fmt.Fprintf(&b, "vendor: %s\n", yamlString(provider))
	fmt.Fprintf(&b, "category: Medical\n")
	fmt.Fprintf(&b, "total_amount: %d\n", data.Amount)
	fmt.Fprintf(&b, "nta_medical:\n")
	fmt.Fprintf(&b, "  patient_name: %s\n", yamlString(patient))
	fmt.Fprintf(&b, "  provider_name: %s\n", yamlString(provider))
	fmt.Fprintf(&b, "  classification:\n")
	fmt.Fprintf(&b, "    treatment: %t\n", isTreatment)
	fmt.Fprintf(&b, "    medicine: %t\n", isMedicine)
	fmt.Fprintf(&b, "    nursing_care: %t\n", isNursing)
	fmt.Fprintf(&b, "    other_medical: %t\n", isOther)
	fmt.Fprintf(&b, "  amount_paid: %d\n", data.Amount)
	fmt.Fprintf(&b, "  insured_copay_amount: %d\n", insuredCopay)
	fmt.Fprintf(&b, "  uninsured_amount: %d\n", uninsured)
	fmt.Fprintf(&b, "  amount_reimbursed: %d\n", reimbursed)
	fmt.Fprintf(&b, "  payment_date: %s\n", yamlString(slashDate))
	fmt.Fprintf(&b, "  form_columns:\n")
	fmt.Fprintf(&b, "    医療を受けた人: %s\n", yamlString(ntaPatient))
	fmt.Fprintf(&b, "    病院・薬局などの名称: %s\n", yamlString(ntaProvider))
	fmt.Fprintf(&b, "    診療・治療: %s\n", yamlString(ntaFlag(isTreatment)))
	fmt.Fprintf(&b, "    医薬品購入: %s\n", yamlString(ntaFlag(isMedicine)))
	fmt.Fprintf(&b, "    介護保険サービス: %s\n", yamlString(ntaFlag(isNursing)))
	fmt.Fprintf(&b, "    その他の医療費: %s\n", yamlString(ntaFlag(isOther)))
	fmt.Fprintf(&b, "    支払った医療費の金額: %d\n", data.Amount)
	fmt.Fprintf(&b, "    左のうち、補填される金額: %s\n", reimbursedCol)
	fmt.Fprintf(&b, "    支払年月日: %s\n", yamlString(slashDate))
	return b.String()
}

// writeMedicalZip bundles the original scan file and its companion YAML metadata into a single .zip archive
func writeMedicalZip(srcPath, zipPath string, data ReceiptData, resolvedDate string) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	zipFile, err := os.OpenFile(zipPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	zw := zip.NewWriter(zipFile)
	stem := strings.TrimSuffix(filepath.Base(zipPath), filepath.Ext(zipPath))
	now := time.Now()

	// 1. Add the original scan file (<stem>.pdf / .jpg / .png)
	scanEntryName := stem + filepath.Ext(srcPath)
	scanHeader, err := zip.FileInfoHeader(srcInfo)
	if err != nil {
		_ = zw.Close()
		_ = zipFile.Close()
		_ = os.Remove(zipPath)
		return err
	}
	scanHeader.Name = scanEntryName
	scanHeader.Method = zip.Deflate
	scanHeader.Modified = now

	scanWriter, err := zw.CreateHeader(scanHeader)
	if err != nil {
		_ = zw.Close()
		_ = zipFile.Close()
		_ = os.Remove(zipPath)
		return err
	}
	if _, err := io.Copy(scanWriter, srcFile); err != nil {
		_ = zw.Close()
		_ = zipFile.Close()
		_ = os.Remove(zipPath)
		return err
	}

	// 2. Add the companion YAML file (<stem>.yaml)
	yamlHeader := &zip.FileHeader{
		Name:     stem + ".yaml",
		Method:   zip.Deflate,
		Modified: now,
	}
	yamlHeader.SetMode(0644)
	yamlWriter, err := zw.CreateHeader(yamlHeader)
	if err != nil {
		_ = zw.Close()
		_ = zipFile.Close()
		_ = os.Remove(zipPath)
		return err
	}
	if _, err := io.WriteString(yamlWriter, formatMedicalYAML(data, resolvedDate)); err != nil {
		_ = zw.Close()
		_ = zipFile.Close()
		_ = os.Remove(zipPath)
		return err
	}

	if err := zw.Close(); err != nil {
		_ = zipFile.Close()
		_ = os.Remove(zipPath)
		return err
	}
	if err := zipFile.Close(); err != nil {
		_ = os.Remove(zipPath)
		return err
	}
	return nil
}

func saveProcessedFile(srcPath string, data ReceiptData) error {
	vendor := sanitizeVendor(data.Vendor)
	date := resolveReceiptDate(data)
	category := sanitizeCategory(data.Category)

	processedDir := filepath.Join(destDir, category)
	if err := os.MkdirAll(processedDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", processedDir, err)
	}

	if category == "Medical" {
		zipFileName := fmt.Sprintf("%s_%s_%d円.zip", date, vendor, data.Amount)
		processedPath := uniquePath(filepath.Join(processedDir, zipFileName))
		if err := writeMedicalZip(srcPath, processedPath, data, date); err != nil {
			return fmt.Errorf("failed to write medical zip archive: %w", err)
		}
		log.Printf("Saved processed file: %s", processedPath)
		return nil
	}

	processedFileName := fmt.Sprintf("%s_%s_%d円%s", date, vendor, data.Amount, filepath.Ext(srcPath))
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
