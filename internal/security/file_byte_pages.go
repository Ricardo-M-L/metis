package security

import (
	"regexp"
	"sort"
	"strings"
)

// NewBytePageCredentialRedactor locates single-line credentials in the complete
// source as well. A byte page may omit a token prefix or assignment key, so the
// ordinary final redactor cannot safely classify that slice alone. Line-based
// callers retain NewFileCredentialRedactor's existing policy.
func NewBytePageCredentialRedactor(source []byte) FileCredentialRedactor {
	out := NewFileCredentialRedactor(source)
	if out.overflow {
		return out
	}
	add := func(start, end int) bool {
		if start < 0 || end <= start || end > len(source) || isStructuredCredentialPlaceholder(source[start:end]) {
			return true
		}
		return out.appendSpan(fileCredentialSpan{start: start, end: end})
	}
	text := string(source)
	if mayContainHighConfidenceSecret(text) {
		compileOnce.Do(compileRules)
		for _, rule := range compiledList {
			matches := rule.re.FindAllSubmatchIndex(source, maxFileCredentialCandidates+1)
			if len(matches) > maxFileCredentialCandidates {
				out.spans = nil
				out.overflow = true
				return out
			}
			for _, match := range matches {
				start, end := match[0], match[1]
				if rule.capturing {
					i := rule.captureGroup * 2
					if i+1 >= len(match) {
						continue
					}
					start, end = match[i], match[i+1]
				}
				if rule.base64URLBoundary && ((start > 0 && isBase64URLByte(source[start-1])) || (end < len(source) && isBase64URLByte(source[end]))) {
					continue
				}
				if !add(start, end) {
					return out
				}
			}
		}
	}
	if mayContainSensitiveTokenField(text) {
		for _, m := range sensitiveTokenRE.FindAllIndex(source, maxFileCredentialCandidates+1) {
			if !add(m[0], m[1]) {
				return out
			}
		}
	}
	if mayContainGenericCredentialSyntax(text) {
		for _, spec := range []struct {
			re              *regexp.Regexp
			key, start, end int
		}{
			{escapedJSONAssignmentRE, 4, 6, 7},
			{jsonAssignmentRE, 4, 3, 1},
			{quotedPlainAssignmentRE, 4, 3, 1},
			{plainAssignmentRE, 2, 5, 1},
			{authorizationHeaderRE, -1, 6, 7},
		} {
			matches := spec.re.FindAllSubmatchIndex(source, maxFileCredentialCandidates+1)
			if len(matches) > maxFileCredentialCandidates {
				out.spans = nil
				out.overflow = true
				return out
			}
			for _, m := range matches {
				if spec.key >= 0 && !isCredentialFieldName(string(source[m[spec.key]:m[spec.key+1]])) {
					continue
				}
				if !add(m[spec.start], m[spec.end]) {
					return out
				}
			}
		}
		for _, re := range []*regexp.Regexp{urlUserinfoRE, databaseNetworkUserinfoRE} {
			matches := re.FindAllSubmatchIndex(source, maxFileCredentialCandidates+1)
			if len(matches) > maxFileCredentialCandidates {
				out.spans = nil
				out.overflow = true
				return out
			}
			for _, m := range matches {
				end := m[0] + strings.LastIndexByte(string(source[m[0]:m[1]]), '@')
				if !add(m[3], end) {
					return out
				}
			}
		}
	}
	sort.Slice(out.spans, func(i, j int) bool { return out.spans[i].start < out.spans[j].start })
	merged := out.spans[:0]
	for _, span := range out.spans {
		if len(merged) > 0 && span.start <= merged[len(merged)-1].end {
			merged[len(merged)-1].end = max(merged[len(merged)-1].end, span.end)
		} else {
			merged = append(merged, span)
		}
	}
	out.spans = merged
	return out
}
