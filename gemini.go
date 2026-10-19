package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"
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

// uploadAndWaitFile uploads path to the Gemini Files API and waits until remote processing completes
func uploadAndWaitFile(ctx context.Context, client *genai.Client, path string) (*genai.File, error) {
	baseName := filepath.Base(path)

	uploadStart := time.Now()
	log.Printf("Uploading to Gemini Files API: %s", baseName)
	upFile, err := client.Files.UploadFromPath(ctx, path, nil)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}

	log.Printf("Uploaded %s -> %s (mime=%s, state=%s, took=%s)",
		baseName, upFile.Name, upFile.MIMEType, upFile.State, time.Since(uploadStart).Round(time.Millisecond))

	pollCtx, pollCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer pollCancel()

	if upFile.State == genai.FileStateProcessing {
		log.Printf("Waiting for Gemini remote file processing: %s (%s)...", baseName, upFile.Name)
	}
	pollStart := time.Now()
	for upFile.State == genai.FileStateProcessing {
		select {
		case <-pollCtx.Done():
			return upFile, fmt.Errorf("interrupted waiting for remote file processing: %w", pollCtx.Err())
		case <-time.After(1 * time.Second):
		}

		upFile, err = client.Files.Get(pollCtx, upFile.Name, nil)
		if err != nil {
			return upFile, fmt.Errorf("check failed state: %w", err)
		}
	}

	if upFile.State != genai.FileStateActive {
		return upFile, fmt.Errorf("file processing failed state: %s", upFile.State)
	}
	if time.Since(pollStart) >= time.Second {
		log.Printf("Remote file ready: %s (took=%s)", upFile.Name, time.Since(pollStart).Round(time.Millisecond))
	}

	return upFile, nil
}

// buildReceiptPrompt constructs the extraction prompt with current date and Japanese Era (Wareki) rules
func buildReceiptPrompt(now time.Time) string {
	currentYear := now.Year()
	reiwaYear := currentYear - 2018

	return fmt.Sprintf(`Analyze this Japanese receipt or certificate and extract all receipt entries matching the response schema.
Today's date is %[1]s (Year %[2]d = Reiwa %[3]d / 令和%[3]d年).

Japanese Era (Wareki) Conversion Rules:
- Reiwa (令和 / 令 / R): Year = 2018 + N (Reiwa 1/元年 = 2019, Reiwa 6 = 2024, Reiwa 7 = 2025, Reiwa 8 / 令和8年 / 令8 / R8 = 2026).
- Heisei (平成 / 平 / H): Year = 1988 + N.
- Read the era year digit carefully on medical receipts (do not confuse 令和%[3]d年 with 令和5年 or 令和6年).
- If the document contains multiple pages for the same receipt/transaction, return only a single entry for that transaction.`,
		now.Format("2006-01-02"), currentYear, reiwaYear)
}

// analyzeReceipt uploads the file to Gemini and extracts receipt data
func analyzeReceipt(ctx context.Context, client *genai.Client, path string) ([]ReceiptData, error) {
	baseName := filepath.Base(path)

	upFile, err := uploadAndWaitFile(ctx, client, path)
	if upFile != nil && upFile.Name != "" {
		defer func(fileName string) {
			delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if _, err := client.Files.Delete(delCtx, fileName, nil); err != nil {
				log.Printf("Failed to delete uploaded file %s: %v", fileName, err)
			}
		}(upFile.Name)
	}
	if err != nil {
		return nil, err
	}

	prompt := buildReceiptPrompt(time.Now())

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
