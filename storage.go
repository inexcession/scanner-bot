package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

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
			data = normalizeMedicalReceipt(data)
			log.Printf("Extracted receipt: %s, patient=%q, vendor=%q, category=%q, amount=%d円 (insured=%d円, uninsured=%d円, treatment=%t, medicine=%t, nursing=%t, other=%t, reimbursed=%d円)",
				dateInfo, data.PatientName, data.Vendor, data.Category, data.Amount,
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

// writeMedicalZip bundles the original scan file and its companion YAML metadata into a single .zip archive
func writeMedicalZip(srcPath, zipPath string, data ReceiptData, resolvedDate string) (retErr error) {
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
	defer func() {
		if retErr != nil {
			_ = zw.Close()
			_ = zipFile.Close()
			_ = os.Remove(zipPath)
		}
	}()

	stem := strings.TrimSuffix(filepath.Base(zipPath), filepath.Ext(zipPath))
	now := time.Now()

	// 1. Add the original scan file (<stem>.pdf / .jpg / .png)
	scanEntryName := stem + filepath.Ext(srcPath)
	scanHeader, err := zip.FileInfoHeader(srcInfo)
	if err != nil {
		return err
	}
	scanHeader.Name = scanEntryName
	scanHeader.Method = zip.Deflate
	scanHeader.Modified = now

	scanWriter, err := zw.CreateHeader(scanHeader)
	if err != nil {
		return err
	}
	if _, err := io.Copy(scanWriter, srcFile); err != nil {
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
		return err
	}
	if _, err := io.WriteString(yamlWriter, formatMedicalYAML(data, resolvedDate)); err != nil {
		return err
	}

	if err := zw.Close(); err != nil {
		return err
	}
	if err := zipFile.Close(); err != nil {
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
