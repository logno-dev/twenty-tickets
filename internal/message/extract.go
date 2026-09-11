// Package message extracts the newest useful section of a plain-text email.
package message

import (
	"regexp"
	"strings"
)

var (
	forwardMarker  = regexp.MustCompile(`(?i)^\s*(?:begin\s+forwarded\s+message:|(?:[-=_]{3,}\s*)?forwarded(?:\s+message)?(?::)?(?:\s*[-=_]{3,})?)\s*$`)
	originalMarker = regexp.MustCompile(`(?i)^\s*-+\s*original message\s*-+\s*$`)
	replyMarker    = regexp.MustCompile(`(?i)^on\s+.+\s+wrote:\s*$`)
	header         = regexp.MustCompile(`(?i)^(from|sent|date|to|cc|bcc|subject|reply-to):\s*`)
	forwardSubject = regexp.MustCompile(`(?i)^\s*(fw|fwd):`)
	blankLines     = regexp.MustCompile(`\n(?:[\t ]*\n){2,}`)
	signoff        = regexp.MustCompile(`(?i)^(best(?: regards)?|kind regards|regards|thanks|thank you|sincerely|cheers|warm regards|respectfully)[,!]?$`)
	mobileFooter   = regexp.MustCompile(`(?i)^(sent from my |sent from mail for |get outlook for |sent using )`)
	legalFooter    = regexp.MustCompile(`(?i)^(confidentiality notice|confidentiality disclaimer|this (?:e-?mail|message) (?:and any attachments )?(?:is|are) confidential)`)
	subjectPrefix  = regexp.MustCompile(`(?i)^\s*(?:re|fw|fwd)\s*:\s*`)
)

// CleanSubject removes only conventional reply/forward prefixes. Repeating the
// match handles subjects such as "Re: Fwd: Re: Printer issue".
func CleanSubject(subject string) string {
	cleaned := strings.TrimSpace(subject)
	for subjectPrefix.MatchString(cleaned) {
		cleaned = subjectPrefix.ReplaceAllString(cleaned, "")
	}
	return strings.TrimSpace(cleaned)
}

// Extract preserves the introductory note and one forwarded body. The bool
// reports whether a forward was recognized. This intentionally favors common
// English mail-client conventions over guessing at arbitrary prose.
func Extract(text, subject string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	var out []string
	forwarded := false
	quoteDepth := 0
	segmentStart := 0
	for i := 0; i < len(lines); i++ {
		line := unquote(lines[i], quoteDepth)
		trim := strings.TrimSpace(line)
		isForward := forwardMarker.MatchString(trim)
		isOriginal := originalMarker.MatchString(trim)
		isHeaders := headerBlock(lines, i, quoteDepth)
		if isForward || isOriginal || isHeaders {
			// Outlook uses the same header layout for replies and forwards;
			// the outer subject disambiguates the first such block.
			if forwarded || (!isForward && !forwardSubject.MatchString(subject)) {
				break
			}
			forwarded = true
			out = stripSignature(out, segmentStart)
			out = append(out, "")
			segmentStart = len(out)
			if !isHeaders {
				i++
			}
			for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}
			if i < len(lines) {
				quoteDepth = depth(lines[i])
			}
			for i < len(lines) && strings.TrimSpace(unquote(lines[i], quoteDepth)) == "" {
				i++
			}
			// Discard transport headers and their folded continuation lines.
			seenHeader := false
			for i < len(lines) {
				v := unquote(lines[i], quoteDepth)
				if header.MatchString(strings.TrimSpace(v)) {
					seenHeader = true
					i++
					continue
				}
				if seenHeader && (strings.HasPrefix(v, " ") || strings.HasPrefix(v, "\t")) && strings.TrimSpace(v) != "" {
					i++
					continue
				}
				break
			}
			i--
			continue
		}
		if strings.HasPrefix(trim, ">") || replyBoundary(lines, i, quoteDepth) {
			break
		}
		// Outlook commonly puts an underline before the next header block.
		if strings.Trim(trim, "_") == "" && len(trim) >= 5 {
			j := i + 1
			for j < len(lines) && strings.TrimSpace(unquote(lines[j], quoteDepth)) == "" {
				j++
			}
			if headerBlock(lines, j, quoteDepth) {
				continue
			}
		}
		out = append(out, line)
	}
	out = stripSignature(out, segmentStart)
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(out, "\n"), "\n\n")), forwarded
}

// stripSignature removes only recognizable footer patterns from one message
// segment. Keeping segment boundaries prevents an outer signature from hiding
// the forwarded message that follows it.
func stripSignature(lines []string, start int) []string {
	if start >= len(lines) {
		return lines
	}
	last := len(lines) - 1
	for last >= start && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	for i := start; i <= last; i++ {
		trim := strings.TrimSpace(lines[i])
		if trim == "--" || mobileFooter.MatchString(trim) || legalFooter.MatchString(trim) {
			return trimTrailing(lines[:i], start)
		}
		if signoff.MatchString(trim) && i > start && strings.TrimSpace(lines[i-1]) == "" && nonEmpty(lines[i+1:last+1]) <= 8 {
			return trimTrailing(lines[:i], start)
		}
	}
	return lines
}

func trimTrailing(lines []string, start int) []string {
	for len(lines) > start && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func nonEmpty(lines []string) int {
	n := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func replyBoundary(lines []string, i, quotes int) bool {
	v := strings.TrimSpace(unquote(lines[i], quotes))
	if !strings.HasPrefix(strings.ToLower(v), "on ") {
		return false
	}
	for j := i; j < len(lines) && j < i+4; j++ {
		if j > i {
			v += " " + strings.TrimSpace(unquote(lines[j], quotes))
		}
		if replyMarker.MatchString(v) {
			return true
		}
	}
	return false
}

func headerBlock(lines []string, i, quotes int) bool {
	if i >= len(lines) || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(unquote(lines[i], quotes))), "from:") {
		return false
	}
	count := 0
	for j := i; j < len(lines) && j < i+8; j++ {
		v := strings.TrimSpace(unquote(lines[j], quotes))
		if v == "" {
			break
		}
		if header.MatchString(v) {
			count++
		}
	}
	return count >= 3
}

func depth(s string) int {
	n := 0
	for strings.HasPrefix(strings.TrimLeft(s, " \t"), ">") {
		s = strings.TrimPrefix(strings.TrimLeft(s, " \t"), ">")
		n++
	}
	return n
}

func unquote(s string, n int) string {
	for ; n > 0; n-- {
		v := strings.TrimLeft(s, " \t")
		if !strings.HasPrefix(v, ">") {
			break
		}
		s = strings.TrimPrefix(strings.TrimPrefix(v, ">"), " ")
	}
	return s
}
