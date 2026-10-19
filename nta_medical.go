package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

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

// normalizeMedicalReceipt applies NTA medical receipt fallback defaults and sanitization
func normalizeMedicalReceipt(data ReceiptData) ReceiptData {
	data.PatientName = sanitizePatientName(data.PatientName)

	provider := strings.TrimSpace(data.Vendor)
	if provider == "" {
		provider = sanitizeVendor(data.Vendor)
	}

	// Default to treatment or medicine if Gemini left all 4 false on a Medical receipt
	if !data.IsTreatment && !data.IsMedicine && !data.IsNursingCare && !data.IsOtherMedical {
		if strings.Contains(provider, "薬局") || strings.Contains(provider, "ドラッグ") || strings.Contains(provider, "ファーマシー") {
			data.IsMedicine = true
		} else {
			data.IsTreatment = true
		}
	}

	if data.InsuredCopayAmount == 0 && data.UninsuredAmount == 0 && data.Amount > 0 {
		data.InsuredCopayAmount = data.Amount
	}

	if data.ReimbursedAmount < 0 {
		data.ReimbursedAmount = 0
	}

	return data
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
		return strconv.Quote(s)
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
	data = normalizeMedicalReceipt(data)
	provider := strings.TrimSpace(data.Vendor)
	if provider == "" {
		provider = sanitizeVendor(data.Vendor)
	}

	reimbursedCol := `""`
	if data.ReimbursedAmount > 0 {
		reimbursedCol = strconv.Itoa(data.ReimbursedAmount)
	}

	slashDate := strings.ReplaceAll(resolvedDate, "-", "/")
	ntaPatient := formatNTAPatient(data.PatientName)
	ntaProvider := formatNTAProvider(provider)

	var b strings.Builder
	fmt.Fprintf(&b, "date: %s\n", yamlString(resolvedDate))
	fmt.Fprintf(&b, "raw_date: %s\n", yamlString(data.RawDate))
	fmt.Fprintf(&b, "vendor: %s\n", yamlString(provider))
	fmt.Fprintf(&b, "category: Medical\n")
	fmt.Fprintf(&b, "total_amount: %d\n", data.Amount)
	fmt.Fprintf(&b, "nta_medical:\n")
	fmt.Fprintf(&b, "  patient_name: %s\n", yamlString(data.PatientName))
	fmt.Fprintf(&b, "  provider_name: %s\n", yamlString(provider))
	fmt.Fprintf(&b, "  classification:\n")
	fmt.Fprintf(&b, "    treatment: %t\n", data.IsTreatment)
	fmt.Fprintf(&b, "    medicine: %t\n", data.IsMedicine)
	fmt.Fprintf(&b, "    nursing_care: %t\n", data.IsNursingCare)
	fmt.Fprintf(&b, "    other_medical: %t\n", data.IsOtherMedical)
	fmt.Fprintf(&b, "  amount_paid: %d\n", data.Amount)
	fmt.Fprintf(&b, "  insured_copay_amount: %d\n", data.InsuredCopayAmount)
	fmt.Fprintf(&b, "  uninsured_amount: %d\n", data.UninsuredAmount)
	fmt.Fprintf(&b, "  amount_reimbursed: %d\n", data.ReimbursedAmount)
	fmt.Fprintf(&b, "  payment_date: %s\n", yamlString(slashDate))
	fmt.Fprintf(&b, "  form_columns:\n")
	fmt.Fprintf(&b, "    医療を受けた人: %s\n", yamlString(ntaPatient))
	fmt.Fprintf(&b, "    病院・薬局などの名称: %s\n", yamlString(ntaProvider))
	fmt.Fprintf(&b, "    診療・治療: %s\n", yamlString(ntaFlag(data.IsTreatment)))
	fmt.Fprintf(&b, "    医薬品購入: %s\n", yamlString(ntaFlag(data.IsMedicine)))
	fmt.Fprintf(&b, "    介護保険サービス: %s\n", yamlString(ntaFlag(data.IsNursingCare)))
	fmt.Fprintf(&b, "    その他の医療費: %s\n", yamlString(ntaFlag(data.IsOtherMedical)))
	fmt.Fprintf(&b, "    支払った医療費の金額: %d\n", data.Amount)
	fmt.Fprintf(&b, "    左のうち、補填される金額: %s\n", reimbursedCol)
	fmt.Fprintf(&b, "    支払年月日: %s\n", yamlString(slashDate))
	return b.String()
}
