package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
)

// parseValue converts a human-readable value string to the internal int64 representation.
func parseValue(limitType, s string) (int64, error) {
	s = strings.TrimSpace(s)

	switch limitType {
	case "cpu":
		// Accept "3600s", "60m", "1h" for seconds
		return parseDuration(s)
	case "ram", "disk", "net", "disk-io-bytes":
		// Accept "512m", "1g", "100k" for bytes
		return parseBytes(s)
	case "disk-io-ops":
		// Plain integer
		return strconv.ParseInt(s, 10, 64)
	case "spending":
		// Accept "10.00" or "10" for USD, store as cents
		return parseCents(s)
	default:
		return strconv.ParseInt(s, 10, 64)
	}
}

func parseDuration(s string) (int64, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("empty value")
	}
	suffix := s[len(s)-1]
	numStr := s[:len(s)-1]

	switch suffix {
	case 's':
		return strconv.ParseInt(numStr, 10, 64)
	case 'm':
		n, err := strconv.ParseInt(numStr, 10, 64)
		return n * 60, err
	case 'h':
		n, err := strconv.ParseInt(numStr, 10, 64)
		return n * 3600, err
	default:
		// Try plain number as seconds
		return strconv.ParseInt(s, 10, 64)
	}
}

func parseBytes(s string) (int64, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("empty value")
	}
	suffix := strings.ToLower(s[len(s)-1:])
	numStr := s[:len(s)-1]

	var multiplier int64 = 1
	switch suffix {
	case "k":
		multiplier = 1024
	case "m":
		multiplier = 1024 * 1024
	case "g":
		multiplier = 1024 * 1024 * 1024
	case "t":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		numStr = s // no suffix, treat as bytes
	}

	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		// Try float (e.g., "1.5g")
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, err
		}
		return int64(f * float64(multiplier)), nil
	}
	return n * multiplier, nil
}

func parseCents(s string) (int64, error) {
	// Parse as float dollars, convert to cents
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(f * 100), nil
}

func formatValue(limitType string, v int64) string {
	if v == 0 {
		return "-"
	}
	switch limitType {
	case "cpu":
		if v >= 3600 {
			return fmt.Sprintf("%dh%dm%ds", v/3600, (v%3600)/60, v%60)
		}
		if v >= 60 {
			return fmt.Sprintf("%dm%ds", v/60, v%60)
		}
		return fmt.Sprintf("%ds", v)
	case "ram", "disk", "net", "disk-io-bytes":
		return formatBytesHuman(v)
	case "spending":
		return fmt.Sprintf("$%.2f", float64(v)/100)
	default:
		return fmt.Sprintf("%d", v)
	}
}

func formatBytesHuman(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
	)
	switch {
	case b >= tb:
		return fmt.Sprintf("%.1fT", float64(b)/float64(tb))
	case b >= gb:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1fM", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1fK", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func getJSONFloat(m map[string]interface{}, key string) float64 {
	if m == nil {
		return 0
	}
	if v, ok := m[key]; ok {
		switch v := v.(type) {
		case float64:
			return v
		case json.Number:
			f, _ := v.Float64()
			return f
		}
	}
	return 0
}

func printLimitsOrUsage(m map[string]interface{}, label string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "TYPE\t%s\n", strings.ToUpper(label))
	types := []string{"cpu", "ram", "net", "disk", "disk-io-bytes", "disk-io-ops", "spending"}
	for _, t := range types {
		v := int64(getJSONFloat(m, t))
		fmt.Fprintf(w, "%s\t%s\n", t, formatValue(t, v))
	}
	w.Flush()
}
