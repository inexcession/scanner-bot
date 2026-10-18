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

	srcFile := filepath.Join(watchTmp, "scan001.pdf")
	if err := os.WriteFile(srcFile, []byte("dummy-pdf-data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate two identical receipts on the same page
	receipts := []ReceiptData{
		{Date: "2026-10-18", Vendor: "テスト医院", Category: "Medical", Amount: 1500},
		{Date: "2026-10-18", Vendor: "テスト医院", Category: "Medical", Amount: 1500},
	}

	saveAndArchive(srcFile, receipts)

	// Verify both processed files exist without overwriting each other
	firstProcessed := filepath.Join(destTmp, "Medical", "2026-10-18_テスト医院_1500円.pdf")
	secondProcessed := filepath.Join(destTmp, "Medical", "2026-10-18_テスト医院_1500円_1.pdf")
	archivedOriginal := filepath.Join(destTmp, "originals", "scan001.pdf")

	for _, p := range []string{firstProcessed, secondProcessed, archivedOriginal} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected file %s to exist, got error: %v", p, err)
		}
	}

	// Verify original was removed from watchTmp
	if _, err := os.Stat(srcFile); !os.IsNotExist(err) {
		t.Errorf("expected source file %s to be moved, but still exists", srcFile)
	}

	// Now test archiving collision when another scan001.pdf arrives
	if err := os.WriteFile(srcFile, []byte("dummy-pdf-data-2"), 0644); err != nil {
		t.Fatal(err)
	}
	archiveOriginalFile(srcFile)
	archivedSecond := filepath.Join(destTmp, "originals", "scan001_1.pdf")
	if _, err := os.Stat(archivedSecond); err != nil {
		t.Errorf("expected collided archive %s to exist, got error: %v", archivedSecond, err)
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
