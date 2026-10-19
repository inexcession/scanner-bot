package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestParseGeminiResponse(t *testing.T) {
	tests := []struct {
		name    string
		jsonStr string
		want    []ReceiptData
		wantErr bool
	}{
		{
			name:    "single object",
			jsonStr: `{"date": "2023-10-27", "vendor": "Test Clinic", "category": "Medical", "total_amount": 1500}`,
			want: []ReceiptData{
				{Date: "2023-10-27", Vendor: "Test Clinic", Category: "Medical", Amount: 1500},
			},
			wantErr: false,
		},
		{
			name:    "array of objects",
			jsonStr: `[{"date": "2023-10-27", "vendor": "Test Clinic", "category": "Medical", "total_amount": 1500}, {"date": "2023-10-28", "vendor": "Grocery Store", "category": "Grocery", "total_amount": 2500}]`,
			want: []ReceiptData{
				{Date: "2023-10-27", Vendor: "Test Clinic", Category: "Medical", Amount: 1500},
				{Date: "2023-10-28", Vendor: "Grocery Store", Category: "Grocery", Amount: 2500},
			},
			wantErr: false,
		},
		{
			name:    "markdown wrapped json",
			jsonStr: "```json\n" + `{"date": "2023-10-27", "vendor": "Test Clinic", "category": "Medical", "total_amount": 1500}` + "\n```",
			want: []ReceiptData{
				{Date: "2023-10-27", Vendor: "Test Clinic", Category: "Medical", Amount: 1500},
			},
			wantErr: false,
		},
		{
			name:    "json with conversational text",
			jsonStr: "Here is your JSON:\n```json\n" + `{"date": "2023-10-27", "vendor": "Test Clinic", "category": "Medical", "total_amount": 1500}` + "\n```\nHope this helps!",
			want: []ReceiptData{
				{Date: "2023-10-27", Vendor: "Test Clinic", Category: "Medical", Amount: 1500},
			},
			wantErr: false,
		},
		{
			name:    "invalid json",
			jsonStr: `{"date": "2023-10-27"`, // missing closing brace
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGeminiResponse(tt.jsonStr)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseGeminiResponse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseGeminiResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsSupportedReceiptFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/scans/receipt.jpg", true},
		{"/scans/receipt.JPEG", true},
		{"/scans/receipt.png", true},
		{"/scans/receipt.PDF", true},
		{"/scans/.DS_Store", false},
		{"/scans/.hidden.jpg", false},
		{"/scans/receipt.txt", false},
		{"/scans/receipt.jpg.tmp", false},
	}

	for _, tt := range tests {
		if got := isSupportedReceiptFile(tt.path); got != tt.want {
			t.Errorf("isSupportedReceiptFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestSanitization(t *testing.T) {
	if got := sanitizeCategory("medical"); got != "Medical" {
		t.Errorf("sanitizeCategory(medical) = %q, want Medical", got)
	}
	if got := sanitizeCategory("../../etc/passwd"); got != "Unsorted" {
		t.Errorf("sanitizeCategory(traversal) = %q, want Unsorted", got)
	}
	if got := sanitizeCategory(""); got != "Unsorted" {
		t.Errorf("sanitizeCategory(empty) = %q, want Unsorted", got)
	}

	if got := sanitizeVendor(" 東京 / クリニック \n\t"); got != "東京-クリニック" {
		t.Errorf("sanitizeVendor() = %q, want 東京-クリニック", got)
	}
	if got := sanitizeVendor("../../../"); got != "Unknown" {
		t.Errorf("sanitizeVendor(dots/slashes) = %q, want Unknown", got)
	}

	if got := sanitizeDate("2026-10-18"); got != "2026-10-18" {
		t.Errorf("sanitizeDate(valid) = %q, want 2026-10-18", got)
	}
	today := time.Now().Format("2006-01-02")
	if got := sanitizeDate("not-a-date"); got != today {
		t.Errorf("sanitizeDate(invalid) = %q, want %q", got, today)
	}

	warekiTests := []struct {
		input string
		want  string
	}{
		{"令和8年10月18日", "2026-10-18"},
		{"令和 8 年 10 月 2 日", "2026-10-02"},
		{"令和８年１０月１５日", "2026-10-15"},
		{"令8.10.18", "2026-10-18"},
		{"R8/10/18", "2026-10-18"},
		{"r8-10-2", "2026-10-02"},
		{"令和元年5月1日", "2019-05-01"},
		{"平成31年4月30日", "2019-04-30"},
		{"8.10.18", "2026-10-18"},
		{"2026年10月18日", "2026-10-18"},
		{"2026/10/2", "2026-10-02"},
	}
	for _, tc := range warekiTests {
		if got := sanitizeDate(tc.input); got != tc.want {
			t.Errorf("sanitizeDate(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	// Verify RawDate overrides an LLM miscalculated Gregorian year (e.g. 2024 instead of 2026 for Reiwa 8)
	misconverted := ReceiptData{
		RawDate:  "令和8年10月2日",
		Date:     "2024-10-02",
		Vendor:   "医療法人社団CVIC大井町心臓クリニック",
		Category: "Medical",
		Amount:   1850,
	}
	if got := resolveReceiptDate(misconverted); got != "2026-10-02" {
		t.Errorf("resolveReceiptDate() = %q, want 2026-10-02", got)
	}
}

func TestDeduplicateReceipts(t *testing.T) {
	input := []ReceiptData{
		{Date: "2024-10-18", Vendor: "川村眼科", Category: "Medical", Amount: 1740},
		{Date: "2024-10-18", Vendor: " 川村眼科 ", Category: "medical", Amount: 1740},
		{Date: "2024-10-18", Vendor: "川村眼科", Category: "Medical", Amount: 1740},
		{Date: "2024-10-18", Vendor: "別の薬局", Category: "Medical", Amount: 850},
	}

	got := deduplicateReceipts(input)
	if len(got) != 2 {
		t.Fatalf("deduplicateReceipts() returned %d items, want 2: %+v", len(got), got)
	}
	if got[0].Vendor != "川村眼科" || got[1].Vendor != "別の薬局" {
		t.Errorf("deduplicateReceipts() unexpected order/items: %+v", got)
	}
}

func TestSaveAndArchiveWithCollisions(t *testing.T) {
	tmpDir := t.TempDir()
	watchTmp := filepath.Join(tmpDir, "watch")
	destTmp := filepath.Join(tmpDir, "dest")
	if err := os.MkdirAll(watchTmp, 0755); err != nil {
		t.Fatal(err)
	}

	oldDest := destDir
	destDir = destTmp
	defer func() { destDir = oldDest }()

	srcFile1 := filepath.Join(watchTmp, "scan001.pdf")
	if err := os.WriteFile(srcFile1, []byte("dummy-pdf-data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate Gemini returning duplicate entries for the same receipt in a single scan
	receipts := []ReceiptData{
		{Date: "2026-10-18", Vendor: "テスト医院", Category: "Medical", Amount: 1500},
		{Date: "2026-10-18", Vendor: "テスト医院", Category: "Medical", Amount: 1500},
	}

	saveAndArchive(srcFile1, receipts)

	// Verify only a single processed file was created for the duplicate entries in the same scan
	firstProcessed := filepath.Join(destTmp, "Medical", "2026-10-18_テスト医院_1500円.pdf")
	secondProcessed := filepath.Join(destTmp, "Medical", "2026-10-18_テスト医院_1500円_1.pdf")
	archivedOriginal := filepath.Join(destTmp, "originals", "scan001.pdf")

	for _, p := range []string{firstProcessed, archivedOriginal} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file %s to exist, got error: %v", p, err)
		}
	}
	if _, err := os.Stat(secondProcessed); !os.IsNotExist(err) {
		t.Errorf("expected duplicate entry within same scan not to create %s", secondProcessed)
	}

	// Verify original was removed from watchTmp
	if _, err := os.Stat(srcFile1); !os.IsNotExist(err) {
		t.Errorf("expected source file %s to be moved, but still exists", srcFile1)
	}

	// Now test a second distinct scan file that produces the same receipt filename & original name
	if err := os.WriteFile(srcFile1, []byte("dummy-pdf-data-2"), 0644); err != nil {
		t.Fatal(err)
	}
	saveAndArchive(srcFile1, []ReceiptData{
		{Date: "2026-10-18", Vendor: "テスト医院", Category: "Medical", Amount: 1500},
	})
	archivedSecond := filepath.Join(destTmp, "originals", "scan001_1.pdf")
	for _, p := range []string{secondProcessed, archivedSecond} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected collided file %s to exist on second scan, got error: %v", p, err)
		}
	}
}

func TestWaitForStableFile(t *testing.T) {
	oldThreshold := stabilityThreshold
	oldPoll := stabilityPollWait
	stabilityThreshold = 30 * time.Millisecond
	stabilityPollWait = 10 * time.Millisecond
	defer func() {
		stabilityThreshold = oldThreshold
		stabilityPollWait = oldPoll
	}()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "receipt.jpg")
	if err := os.WriteFile(filePath, []byte("image-bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Normal stabilization
	if err := waitForStableFile(context.Background(), filePath); err != nil {
		t.Errorf("waitForStableFile() unexpected error = %v", err)
	}

	// 2. Context cancellation
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForStableFile(ctx, filePath); err == nil {
		t.Errorf("waitForStableFile() expected error on canceled context, got nil")
	}

	// 3. Missing file
	if err := waitForStableFile(context.Background(), filepath.Join(tmpDir, "nonexistent.jpg")); err == nil {
		t.Errorf("waitForStableFile() expected error on nonexistent file, got nil")
	}
}

func TestEnqueueFileIgnoresMissingOrDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	missingPath := filepath.Join(tmpDir, "already_archived.pdf")

	// Should not enqueue a file that no longer exists (e.g. trailing fsnotify event after move)
	enqueueFile(context.Background(), nil, make(chan struct{}, 1), nil, missingPath)
	if _, loaded := activeFiles.Load(missingPath); loaded {
		t.Errorf("expected missing file %s not to be stored in activeFiles", missingPath)
	}
}

