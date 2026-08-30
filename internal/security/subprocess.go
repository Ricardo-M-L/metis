package security

import (
	"bytes"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// subprocessEnvExact is deliberately an allow-list. Model-controlled child
// processes must not inherit the full Metis environment: provider keys,
// connector credentials and authentication-agent sockets frequently live
// there under names that a block-list cannot predict. Keep only process,
// locale and language-toolchain settings needed to start ordinary developer
// tools. A caller may append its own non-secret, trusted values via explicit.
var subprocessEnvExact = map[string]struct{}{
	"PATH": {}, "HOME": {}, "PWD": {},
	"TMPDIR": {}, "TMP": {}, "TEMP": {},
	"USER": {}, "LOGNAME": {}, "SHELL": {},
	"LANG": {}, "LANGUAGE": {}, "TERM": {}, "COLORTERM": {},
	"NO_COLOR": {}, "FORCE_COLOR": {}, "CI": {}, "TZ": {},

	// Windows process/bootstrap essentials. Provider and auth variables remain
	// absent even on Windows; these entries only locate the OS and user home.
	"SYSTEMROOT": {}, "WINDIR": {}, "COMSPEC": {}, "PATHEXT": {},
	"USERPROFILE": {}, "HOMEDRIVE": {}, "HOMEPATH": {},
	"APPDATA": {}, "LOCALAPPDATA": {}, "PROGRAMDATA": {},
	"PROGRAMFILES": {}, "PROGRAMFILES(X86)": {},
	"OS": {}, "PROCESSOR_ARCHITECTURE": {}, "NUMBER_OF_PROCESSORS": {},

	// Go.
	"GOPATH": {}, "GOROOT": {}, "GOMODCACHE": {}, "GOCACHE": {},
	"GOENV": {}, "GOFLAGS": {}, "GOTOOLCHAIN": {},
	"GOPROXY": {}, "GOPRIVATE": {}, "GONOPROXY": {}, "GONOSUMDB": {},

	// Credential-free enterprise proxy discovery. Values containing URL
	// userinfo or credential-shaped query/assignment data are dropped below.
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {}, "NO_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "all_proxy": {}, "no_proxy": {},

	// Python / Node / other supported RunCode and LSP runtimes.
	"PYTHONPATH": {}, "PYTHONHOME": {}, "VIRTUAL_ENV": {}, "CONDA_PREFIX": {},
	"PYENV_ROOT": {},
	"NODE_PATH":  {}, "NVM_DIR": {}, "VOLTA_HOME": {}, "PNPM_HOME": {}, "BUN_INSTALL": {},
	"NPM_CONFIG_CACHE": {}, "NPM_CONFIG_PREFIX": {},
	"CARGO_HOME": {}, "RUSTUP_HOME": {},
	"ASDF_DIR": {}, "ASDF_DATA_DIR": {},
	"MISE_DATA_DIR": {}, "MISE_CACHE_DIR": {}, "MISE_CONFIG_DIR": {}, "MISE_STATE_DIR": {},
	"R_LIBS": {}, "R_LIBS_USER": {}, "RUBYLIB": {}, "GEM_HOME": {}, "GEM_PATH": {},
	"PERL5LIB": {}, "PHPRC": {}, "JAVA_HOME": {}, "CLASSPATH": {},

	// Build / SDK and certificate discovery. Dynamic-loader injection variables
	// (LD_PRELOAD, DYLD_*) are intentionally absent.
	"SDKROOT": {}, "DEVELOPER_DIR": {}, "CPATH": {}, "LIBRARY_PATH": {},
	"PKG_CONFIG_PATH": {}, "SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
	"REQUESTS_CA_BUNDLE": {}, "CURL_CA_BUNDLE": {}, "NODE_EXTRA_CA_CERTS": {},

	// Per-user cache/config locations used by language servers and Chromium.
	"XDG_CACHE_HOME": {}, "XDG_CONFIG_HOME": {}, "XDG_DATA_HOME": {}, "XDG_STATE_HOME": {},
	"__CF_USER_TEXT_ENCODING": {}, "COMMAND_MODE": {}, "MALLOCNANOZONE": {},
}

var (
	envNameRE                 = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	jsonAssignmentRE          = regexp.MustCompile(`(?i)("([A-Za-z_][A-Za-z0-9_.-]*)"\s*:\s*)"(?:\\.|[^"\\\r\n])*"`)
	escapedJSONAssignmentRE   = regexp.MustCompile(`(?i)(\\"([A-Za-z_][A-Za-z0-9_.-]*)\\"\s*:\s*\\")((?:[^"\\\r\n]|\\\\(?:\\"|\\.|[^"\r\n])|\\[^"])*)(\\")`)
	quotedPlainAssignmentRE   = regexp.MustCompile(`(?i)((?:"|')([A-Za-z_][A-Za-z0-9_.-]*)(?:"|')\s*[=:]\s*)(?:"(?:\\.|[^"\\\r\n])*"|'(?:\\.|[^'\\\r\n])*'|[^\s,;"'{}\[\]]+)`)
	authorizationHeaderRE     = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\s*:\s*)([A-Za-z][A-Za-z0-9_-]*)\s+([^\s,;]+)`)
	plainAssignmentRE         = regexp.MustCompile(`(?i)\b([A-Za-z_][A-Za-z0-9_.-]*)(\s*[=:]\s*)(?:"(?:\\.|[^"\\\r\n])*"|'(?:\\.|[^'\\\r\n])*'|[^\s,;"'{}\[\]]+)`)
	urlUserinfoRE             = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^/@\s:?#]+:)[^/@\s?#]+@`)
	databaseNetworkUserinfoRE = regexp.MustCompile(`(?i)\b([A-Za-z0-9._-]+:)[^@\s]+@(tcp|unix)\(`)

	// fileCredentialStructureRE deliberately matches only credential forms
	// whose key/header context can be removed by line-oriented pagination. A
	// complete single-line token remains the responsibility of
	// RedactSubprocessText. Keeping these alternatives in one regexp makes the
	// clean-file path one linear scan, even for Read's 256 MiB upper bound.
	fileCredentialStructureRE = regexp.MustCompile(
		`(?:^|\n)(?P<pem_begin>(?:\x{FEFF})?[ \t]*-----BEGIN[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----[ \t]*\r?\n)` +
			`(?P<pem_body>(?s:.+?))(?:(?:\r?\n[ \t]*-----END[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----[ \t]*(?:\r?\n|$))|\z)` +
			`|(?P<escaped_json>\\"(?P<escaped_json_key>[A-Za-z_][A-Za-z0-9_.-]*)\\"` +
			`(?:[ \t\r]*\n[ \t\r\n]*:[ \t\r\n]*|[ \t]*:[ \t\r]*\n[ \t\r\n]*)` +
			`\\"(?P<escaped_json_value>(?:[^"\\\r\n]|\\\\(?:\\"|\\.|[^"\r\n])|\\[^"])*)\\")` +
			`|(?P<json>"(?P<json_key>[A-Za-z_][A-Za-z0-9_.-]*)"` +
			`(?:[ \t\r]*\n[ \t\r\n]*:[ \t\r\n]*|[ \t]*:[ \t\r]*\n[ \t\r\n]*)` +
			`"(?P<json_value>(?:\\.|[^"\\\r\n])*)")` +
			`|(?P<triple_dq>\b(?P<triple_dq_key>[A-Za-z_][A-Za-z0-9_.-]*)[ \t]*(?:=|:)[ \t]*"""[ \t]*\r?\n` +
			`(?P<triple_dq_value>(?s:.+?))(?:(?:""")|\z))` +
			`|(?P<triple_sq>\b(?P<triple_sq_key>[A-Za-z_][A-Za-z0-9_.-]*)[ \t]*(?:=|:)[ \t]*'''[ \t]*\r?\n` +
			`(?P<triple_sq_value>(?s:.+?))(?:(?:''')|\z))` +
			`|(?P<multiline_dq>\b(?P<multiline_dq_key>[A-Za-z_][A-Za-z0-9_.-]*)[ \t]*(?:=|:)[ \t]*"` +
			`(?P<multiline_dq_value>(?:\\.|[^"\\\r\n])*\r?\n(?:\\.|[^"\\])*)")` +
			`|(?P<multiline_sq>\b(?P<multiline_sq_key>[A-Za-z_][A-Za-z0-9_.-]*)[ \t]*(?:=|:)[ \t]*'` +
			`(?P<multiline_sq_value>(?:\\.|[^'\\\r\n])*\r?\n(?:\\.|[^'\\])*)')` +
			`|(?P<quoted_plain>(?:"(?P<quoted_plain_dq_key>[A-Za-z_][A-Za-z0-9_.-]*)"|'(?P<quoted_plain_sq_key>[A-Za-z_][A-Za-z0-9_.-]*)')` +
			`(?:[ \t\r]*\n[ \t\r\n]*(?:=|:)[ \t\r\n]*|[ \t]*(?:=|:)[ \t\r]*\n[ \t\r\n]*)` +
			`(?:"(?P<quoted_plain_dq>(?:\\.|[^"\\\r\n])*)"|'(?P<quoted_plain_sq>(?:\\.|[^'\\\r\n])*)'|(?:(?:(?:[ \t]*#[^\r\n]*|[ \t]*)\r?\n)*[ \t]*)(?P<quoted_plain_raw>[^\r\n]+)))` +
			`|(?P<yaml_block>\b(?P<yaml_block_key>[A-Za-z_][A-Za-z0-9_.-]*)[ \t]*:[ \t]*[|>][0-9+-]*[ \t]*(?:#[^\r\n]*)?\r?\n` +
			`(?P<yaml_block_value>(?:(?:[ \t]+[^\r\n]*|[ \t]*)\r?\n)*(?:[ \t]+[^\r\n]*)?))` +
			`|(?P<authorization>(?i:\b(?:proxy-)?authorization)[ \t]*:` +
			`(?:[ \t\r]*\n[ \t\r\n]*(?P<authorization_scheme_after>[A-Za-z][A-Za-z0-9_-]*)[ \t\r\n]+` +
			`|[ \t]*(?P<authorization_scheme_before>[A-Za-z][A-Za-z0-9_-]*)[ \t\r]*\n[ \t\r\n]*)` +
			`(?P<authorization_value>[^\s,;]+))` +
			`|(?P<bare_bearer>(?i:\bbearer)[ \t\r]*\n[ \t\r\n]*(?P<bare_bearer_value>[^\s,;]+))` +
			`|(?P<plain>\b(?P<plain_key>[A-Za-z_][A-Za-z0-9_.-]*)` +
			`(?:[ \t\r]*\n[ \t\r\n]*(?:=|:)[ \t\r\n]*|[ \t]*(?:=|:)[ \t\r]*\n[ \t\r\n]*)` +
			`(?:"(?P<plain_dq>(?:\\.|[^"\\\r\n])*)"|'(?P<plain_sq>(?:\\.|[^'\\\r\n])*)'|(?:(?:(?:[ \t]*#[^\r\n]*|[ \t]*)\r?\n)*[ \t]*)(?P<plain_raw>[^\r\n]+)))`,
	)
)

const (
	maxFileCredentialSpans      = 4096
	maxFileCredentialCandidates = 8192
)

type fileCredentialSpan struct {
	start int
	end   int
}

// FileCredentialRedactor records source byte ranges whose credential context
// spans lines. It holds no copy or reference to the source bytes. Callers can
// therefore scan a large pinned file once, then redact only the requested
// lines without retaining per-line strings or maps.
type FileCredentialRedactor struct {
	spans    []fileCredentialSpan
	overflow bool
}

// NewFileCredentialRedactor classifies a complete, pinned file snapshot. The
// detector is intentionally narrower than Redact: single-line values are safe
// to handle at the final output boundary, while these source ranges would lose
// their key, header, or PEM markers after Read/Grep pagination.
func NewFileCredentialRedactor(source []byte) FileCredentialRedactor {
	var out FileCredentialRedactor
	searchFrom, ok := nextMultilineCredentialSearchStart(source, 0)
	if !ok {
		return out
	}
	candidates := 0
	for searchFrom < len(source) {
		match := fileCredentialStructureRE.FindSubmatchIndex(source[searchFrom:])
		if match == nil {
			break
		}
		candidates++
		if candidates > maxFileCredentialCandidates {
			out.spans = nil
			out.overflow = true
			return out
		}
		for i, offset := range match {
			if offset >= 0 {
				match[i] = offset + searchFrom
			}
		}
		matchEnd := match[1]
		if matchEnd <= searchFrom {
			matchEnd = searchFrom + 1
		}

		if begin, ok := fileCredentialSubmatch(match, "pem_begin"); ok {
			// A ^ anchor also treats the beginning of a sliced search buffer as
			// line-start. Reject that synthetic boundary and resume immediately
			// after it so a later real PEM marker is still discoverable.
			if begin.start > 0 && source[begin.start-1] != '\n' {
				searchFrom, ok = nextMultilineCredentialSearchStart(source, begin.start+1)
				if !ok {
					return out
				}
				continue
			}
			if body, found := fileCredentialSubmatch(match, "pem_body"); found {
				if !out.appendSpan(body) {
					return out
				}
			}
			searchFrom, ok = nextMultilineCredentialSearchStart(source, matchEnd)
			if !ok {
				return out
			}
			continue
		}

		var value fileCredentialSpan
		var key fileCredentialSpan
		var found bool
		matchedPlain := false
		switch {
		case fileCredentialGroupMatched(match, "escaped_json"):
			key, _ = fileCredentialSubmatch(match, "escaped_json_key")
			value, found = fileCredentialSubmatch(match, "escaped_json_value")
		case fileCredentialGroupMatched(match, "json"):
			key, _ = fileCredentialSubmatch(match, "json_key")
			value, found = fileCredentialSubmatch(match, "json_value")
		case fileCredentialGroupMatched(match, "triple_dq"):
			key, _ = fileCredentialSubmatch(match, "triple_dq_key")
			value, found = fileCredentialSubmatch(match, "triple_dq_value")
		case fileCredentialGroupMatched(match, "triple_sq"):
			key, _ = fileCredentialSubmatch(match, "triple_sq_key")
			value, found = fileCredentialSubmatch(match, "triple_sq_value")
		case fileCredentialGroupMatched(match, "multiline_dq"):
			key, _ = fileCredentialSubmatch(match, "multiline_dq_key")
			value, found = fileCredentialSubmatch(match, "multiline_dq_value")
		case fileCredentialGroupMatched(match, "multiline_sq"):
			key, _ = fileCredentialSubmatch(match, "multiline_sq_key")
			value, found = fileCredentialSubmatch(match, "multiline_sq_value")
		case fileCredentialGroupMatched(match, "quoted_plain"):
			matchedPlain = true
			for _, name := range []string{"quoted_plain_dq_key", "quoted_plain_sq_key"} {
				if key, found = fileCredentialSubmatch(match, name); found {
					break
				}
			}
			found = false
			for _, name := range []string{"quoted_plain_dq", "quoted_plain_sq", "quoted_plain_raw"} {
				if value, found = fileCredentialSubmatch(match, name); found {
					break
				}
			}
		case fileCredentialGroupMatched(match, "yaml_block"):
			key, _ = fileCredentialSubmatch(match, "yaml_block_key")
			value, found = fileCredentialSubmatch(match, "yaml_block_value")
		case fileCredentialGroupMatched(match, "authorization"):
			value, found = fileCredentialSubmatch(match, "authorization_value")
		case fileCredentialGroupMatched(match, "bare_bearer"):
			value, found = fileCredentialSubmatch(match, "bare_bearer_value")
		case fileCredentialGroupMatched(match, "plain"):
			matchedPlain = true
			key, _ = fileCredentialSubmatch(match, "plain_key")
			for _, name := range []string{"plain_dq", "plain_sq", "plain_raw"} {
				if value, found = fileCredentialSubmatch(match, name); found {
					break
				}
			}
		}
		accepted := found && (key.end == 0 || isCredentialFieldName(string(source[key.start:key.end]))) &&
			!isStructuredCredentialPlaceholder(source[value.start:value.end])
		if accepted {
			if !out.appendSpan(value) {
				return out
			}
			if matchedPlain && value.start > searchFrom {
				// An accepted scalar can itself introduce a cross-line form,
				// notably password:\nBearer\nTOKEN. Search again from the
				// scalar start while retaining the span already recorded.
				searchFrom, ok = nextMultilineCredentialSearchStart(source, value.start)
				if !ok {
					return out
				}
				continue
			}
			searchFrom, ok = nextMultilineCredentialSearchStart(source, matchEnd)
			if !ok {
				return out
			}
			continue
		}
		// An ignored broad assignment can contain the beginning of a real
		// credential form (for example metadata:\n password:\n secret or
		// foo:\n Bearer\n TOKEN). Resume just after its key/start rather than
		// swallowing that overlapping candidate with matchEnd.
		restartFrom := match[0] + 1
		if key.end > restartFrom {
			restartFrom = key.end
		}
		if restartFrom <= searchFrom {
			restartFrom = searchFrom + 1
		}
		searchFrom, ok = nextMultilineCredentialSearchStart(source, restartFrom)
		if !ok {
			return out
		}
	}
	return out
}

func nextMultilineCredentialSearchStart(source []byte, from int) (int, bool) {
	if from < 0 {
		from = 0
	}
	if from >= len(source) {
		return 0, false
	}
	relative, ok := firstMultilineCredentialCandidate(source[from:])
	if !ok {
		return 0, false
	}
	start := from + relative
	// JSON keys may begin with \" two bytes before the identifier. Starting
	// nearby retains every syntactic opener without rescanning a clean prefix.
	if start > from+2 {
		start -= 2
	} else {
		start = from
	}
	return start, true
}

func firstMultilineCredentialCandidate(source []byte) (int, bool) {
	if len(source) == 0 {
		return 0, false
	}
	// PEM markers are rare and case-sensitive by specification. A broad byte
	// prefilter is intentional; the anchored union branch performs validation.
	pemStart := firstPEMLineCandidate(source)
	for i := 0; i < len(source); {
		if pemStart >= 0 && i >= pemStart {
			return pemStart, true
		}
		if !isASCIIIdentifierStart(source[i]) {
			i++
			continue
		}
		start := i
		i++
		for i < len(source) && isASCIIIdentifierContinue(source[i]) {
			i++
		}
		name := source[start:i]
		if asciiEqualFoldBytes(name, "bearer") && bearerCrossesLine(source, i) {
			if pemStart >= 0 && pemStart < start {
				return pemStart, true
			}
			return start, true
		}
		if !credentialNameMayMatch(name) || !isCredentialFieldNameBytes(name) {
			continue
		}
		if credentialFieldCrossesLine(source, i, asciiEqualFoldBytes(name, "authorization") ||
			asciiEqualFoldBytes(name, "proxy-authorization")) {
			if pemStart >= 0 && pemStart < start {
				return pemStart, true
			}
			return start, true
		}
	}
	if pemStart >= 0 {
		return pemStart, true
	}
	return 0, false
}

func firstPEMLineCandidate(source []byte) int {
	for searchFrom := 0; searchFrom < len(source); {
		relative := bytes.Index(source[searchFrom:], []byte("-----BEGIN"))
		if relative < 0 {
			return -1
		}
		marker := searchFrom + relative
		lineStart := bytes.LastIndexByte(source[:marker], '\n') + 1
		prefix := source[lineStart:marker]
		if bytes.HasPrefix(prefix, []byte{0xef, 0xbb, 0xbf}) {
			prefix = prefix[3:]
		}
		validPrefix := true
		for _, b := range prefix {
			if b != ' ' && b != '\t' {
				validPrefix = false
				break
			}
		}
		lineEnd := len(source)
		if newline := bytes.IndexByte(source[marker:], '\n'); newline >= 0 {
			lineEnd = marker + newline
		}
		line := bytes.Trim(source[marker:lineEnd], " \t\r")
		if validPrefix && bytes.Contains(line, []byte("PRIVATE KEY")) && bytes.HasSuffix(line, []byte("-----")) {
			return lineStart
		}
		searchFrom = marker + 1
	}
	return -1
}

func credentialFieldCrossesLine(source []byte, pos int, authorization bool) bool {
	// JSON and escaped-JSON keys close with " or \" before the colon.
	if pos < len(source) && source[pos] == '"' {
		pos++
	} else if pos < len(source) && source[pos] == '\'' {
		pos++
	} else if pos+1 < len(source) && source[pos] == '\\' && source[pos+1] == '"' {
		pos += 2
	}
	pos, newlineBeforeSeparator := skipCredentialWhitespace(source, pos)
	if pos >= len(source) || (source[pos] != ':' && source[pos] != '=') {
		return false
	}
	pos++

	// YAML block scalars put their content on following indented lines even
	// though the |/> indicator itself is on the key line.
	for pos < len(source) && (source[pos] == ' ' || source[pos] == '\t' || source[pos] == '\r') {
		pos++
	}
	if pos < len(source) && (source[pos] == '|' || source[pos] == '>') {
		for pos < len(source) && source[pos] != '\n' {
			pos++
		}
		return pos < len(source)
	}
	if pos < len(source) && (source[pos] == '"' || source[pos] == '\'') &&
		quotedCredentialValueCrossesLine(source, pos) {
		return true
	}

	pos, newlineAfterSeparator := skipCredentialWhitespace(source, pos)
	if newlineBeforeSeparator || newlineAfterSeparator {
		return true
	}
	if !authorization {
		return false
	}
	// Authorization: Bearer\nTOKEN loses its fixed header context even
	// though the first value token starts on the header line.
	for pos < len(source) && !isCredentialWhitespace(source[pos]) {
		pos++
	}
	_, newlineBeforePayload := skipCredentialWhitespace(source, pos)
	return newlineBeforePayload
}

func quotedCredentialValueCrossesLine(source []byte, pos int) bool {
	quote := source[pos]
	triple := pos+2 < len(source) && source[pos+1] == quote && source[pos+2] == quote
	if triple {
		pos += 3
		for pos < len(source) {
			if source[pos] == '\n' {
				return true
			}
			if pos+2 < len(source) && source[pos] == quote && source[pos+1] == quote && source[pos+2] == quote {
				return false
			}
			pos++
		}
		return false
	}
	for pos++; pos < len(source); pos++ {
		if source[pos] == '\n' {
			return true
		}
		if source[pos] == '\\' && pos+1 < len(source) {
			pos++
			continue
		}
		if source[pos] == quote {
			return false
		}
	}
	return false
}

func bearerCrossesLine(source []byte, pos int) bool {
	if pos >= len(source) || !isCredentialWhitespace(source[pos]) {
		return false
	}
	pos, sawNewline := skipCredentialWhitespace(source, pos)
	return sawNewline && pos < len(source)
}

func skipCredentialWhitespace(source []byte, pos int) (int, bool) {
	sawNewline := false
	for pos < len(source) && isCredentialWhitespace(source[pos]) {
		if source[pos] == '\n' {
			sawNewline = true
		}
		pos++
	}
	return pos, sawNewline
}

func isCredentialWhitespace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\f' || b == '\v'
}

func isASCIIIdentifierStart(b byte) bool {
	return b == '_' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

func isASCIIIdentifierContinue(b byte) bool {
	return isASCIIIdentifierStart(b) || b >= '0' && b <= '9' || b == '.' || b == '-'
}

func credentialNameMayMatch(name []byte) bool {
	for _, marker := range []string{
		"api", "token", "secret", "password", "passwd", "private", "credential",
		"authorization", "bearer", "cookie", "assertion",
	} {
		if containsASCIIFoldBytes(name, marker) {
			return true
		}
	}
	return asciiEqualFoldBytes(name, "pat") || hasASCIIFoldSuffix(name, "_pat") ||
		hasASCIIFoldSuffix(name, "-pat") || hasASCIIFoldSuffix(name, ".pat")
}

func isCredentialFieldNameBytes(name []byte) bool {
	for _, exact := range []string{
		"api_key", "apikey", "token", "access_token", "refresh_token", "id_token",
		"subject_token", "assertion", "client_assertion", "secret", "client_secret",
		"password", "passwd", "private_key", "credential", "credentials",
		"authorization", "bearer", "cookie", "pat",
		"accesstoken", "refreshtoken", "idtoken", "subjecttoken", "clientsecret",
		"clientassertion", "privatekey",
	} {
		if credentialNameEqualsNormalized(name, exact) {
			return true
		}
	}
	for _, suffix := range []string{
		"_api_key", "_apikey", "_token", "_secret", "_password", "_passwd",
		"_private_key", "_credential", "_credentials", "_authorization", "_cookie", "_pat",
	} {
		if credentialNameHasNormalizedSuffix(name, suffix) {
			return true
		}
	}
	for _, suffix := range []string{
		"ApiKey", "Token", "Secret", "Password", "Passwd", "PrivateKey",
		"Credential", "Credentials", "Authorization", "Cookie", "Pat",
	} {
		if credentialNameHasCamelSuffix(name, suffix) {
			return true
		}
	}
	segmentStart := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && normalizedCredentialNameByte(name[i]) != '_' {
			continue
		}
		segment := name[segmentStart:i]
		if credentialNameEqualsNormalized(segment, "secret") ||
			credentialNameEqualsNormalized(segment, "password") ||
			credentialNameEqualsNormalized(segment, "passwd") {
			return true
		}
		segmentStart = i + 1
	}
	return false
}

func credentialNameHasCamelSuffix(name []byte, suffix string) bool {
	if len(name) <= len(suffix) {
		return false
	}
	start := len(name) - len(suffix)
	if name[start] < 'A' || name[start] > 'Z' {
		return false
	}
	return asciiEqualFoldBytes(name[start:], suffix)
}

func credentialNameEqualsNormalized(name []byte, normalized string) bool {
	if len(name) != len(normalized) {
		return false
	}
	for i, b := range name {
		if normalizedCredentialNameByte(b) != normalized[i] {
			return false
		}
	}
	return true
}

func credentialNameHasNormalizedSuffix(name []byte, suffix string) bool {
	if len(name) < len(suffix) {
		return false
	}
	return credentialNameEqualsNormalized(name[len(name)-len(suffix):], suffix)
}

func normalizedCredentialNameByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		b += 'a' - 'A'
	}
	if b == '-' || b == '.' {
		return '_'
	}
	return b
}

func containsASCIIFoldBytes(data []byte, needle string) bool {
	if len(needle) > len(data) {
		return false
	}
	for i := 0; i+len(needle) <= len(data); i++ {
		if asciiEqualFoldBytes(data[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func hasASCIIFoldSuffix(data []byte, suffix string) bool {
	return len(data) >= len(suffix) && asciiEqualFoldBytes(data[len(data)-len(suffix):], suffix)
}

func asciiEqualFoldBytes(data []byte, value string) bool {
	if len(data) != len(value) {
		return false
	}
	for i, b := range data {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		v := value[i]
		if v >= 'A' && v <= 'Z' {
			v += 'a' - 'A'
		}
		if b != v {
			return false
		}
	}
	return true
}

func (r *FileCredentialRedactor) appendSpan(span fileCredentialSpan) bool {
	if span.end <= span.start {
		return true
	}
	if len(r.spans) >= maxFileCredentialSpans {
		// Bound attacker-controlled metadata. RedactLineAt masks every emitted
		// line in this rare case, so exceeding the bound fails closed.
		r.spans = nil
		r.overflow = true
		return false
	}
	r.spans = append(r.spans, span)
	return true
}

func fileCredentialGroupMatched(match []int, name string) bool {
	_, ok := fileCredentialSubmatch(match, name)
	return ok
}

func fileCredentialSubmatch(match []int, name string) (fileCredentialSpan, bool) {
	i := fileCredentialStructureRE.SubexpIndex(name)
	if i < 0 || 2*i+1 >= len(match) || match[2*i] < 0 {
		return fileCredentialSpan{}, false
	}
	return fileCredentialSpan{start: match[2*i], end: match[2*i+1]}, true
}

// HasRedactions reports whether rendering any source line may differ from the
// pinned file. A redacted Read is a partial view and must not grant whole-file
// Write authority based on content the model never saw.
func (r FileCredentialRedactor) HasRedactions() bool {
	return r.overflow || len(r.spans) > 0
}

// RedactLineAt redacts the intersections between one source line and the
// structural credential spans. lineStart is the byte offset of line within the
// exact source passed to NewFileCredentialRedactor.
func (r FileCredentialRedactor) RedactLineAt(lineStart int, line string) string {
	if !r.HasRedactions() {
		return line
	}
	if r.overflow || lineStart < 0 {
		return "[REDACTED]"
	}
	lineEnd := lineStart + len(line)
	i := sort.Search(len(r.spans), func(i int) bool { return r.spans[i].end > lineStart })
	if i == len(r.spans) || r.spans[i].start >= lineEnd {
		return line
	}

	var b strings.Builder
	b.Grow(len(line))
	last := 0
	for ; i < len(r.spans) && r.spans[i].start < lineEnd; i++ {
		start := r.spans[i].start - lineStart
		end := r.spans[i].end - lineStart
		if start < 0 {
			start = 0
		}
		if end > len(line) {
			end = len(line)
		}
		if end <= start || end <= last {
			continue
		}
		if start < last {
			start = last
		}
		b.WriteString(line[last:start])
		b.WriteString("[REDACTED]")
		last = end
	}
	if last == 0 {
		return line
	}
	b.WriteString(line[last:])
	return b.String()
}

func isStructuredCredentialPlaceholder(value []byte) bool {
	s := strings.TrimSpace(string(value))
	if s == "" {
		return true
	}
	lower := strings.ToLower(s)
	switch lower {
	case "[redacted]", "<redacted>", "redacted":
		return true
	}
	if strings.Trim(s, "*") == "" {
		return true
	}
	if strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}") {
		return true
	}
	if strings.HasPrefix(s, "{{") && strings.HasSuffix(s, "}}") {
		return true
	}
	if strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") {
		return true
	}
	if strings.HasPrefix(s, "$") && len(s) > 1 && envNameRE.MatchString(s[1:]) {
		return true
	}
	return false
}

var proxySubprocessEnv = map[string]struct{}{
	"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {},
	"http_proxy": {}, "https_proxy": {}, "all_proxy": {},
	"GOPROXY": {},
}

// toolchainPathSubprocessEnv contains location-only variables required to
// discover per-user language toolchains. They are intentionally distinct from
// credential/config-file variables such as NPM_TOKEN, CARGO_REGISTRIES_*_TOKEN,
// NPM_CONFIG_USERCONFIG and *_AUTH_TOKEN, none of which are inherited.
var toolchainPathSubprocessEnv = map[string]struct{}{
	"PYENV_ROOT": {}, "NPM_CONFIG_PREFIX": {},
	"CARGO_HOME": {}, "RUSTUP_HOME": {},
	"ASDF_DIR": {}, "ASDF_DATA_DIR": {},
	"MISE_DATA_DIR": {}, "MISE_CACHE_DIR": {}, "MISE_CONFIG_DIR": {}, "MISE_STATE_DIR": {},
}

// RestrictedSubprocessEnv returns the small environment shared by Metis child
// processes. Parent values are allow-listed; explicit values are trusted
// application/config additions and replace an inherited value with the same
// name. Agent markers are forced last so child tools stay non-interactive.
func RestrictedSubprocessEnv(parent []string, explicit ...string) []string {
	out := make([]string, 0, len(subprocessEnvExact)+len(explicit)+3)
	for _, kv := range parent {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		canonicalName := canonicalSubprocessEnvName(name)
		_, allowed := subprocessEnvExact[canonicalName]
		if !allowed && !strings.HasPrefix(canonicalName, "LC_") {
			continue
		}
		_, value, _ := strings.Cut(kv, "=")
		if !safeInheritedSubprocessValue(canonicalName, value) {
			continue
		}
		out = replaceSubprocessEnv(out, name, kv)
	}
	for _, kv := range explicit {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || !envNameRE.MatchString(name) {
			continue
		}
		out = replaceSubprocessEnv(out, name, kv)
	}
	out = replaceSubprocessEnv(out, "AGENT", "AGENT=metis")
	out = replaceSubprocessEnv(out, "AI_AGENT", "AI_AGENT=metis")
	out = replaceSubprocessEnv(out, "METIS", "METIS=1")
	return out
}

func safeInheritedSubprocessValue(name, value string) bool {
	if _, isToolchainPath := toolchainPathSubprocessEnv[name]; isToolchainPath {
		// These bindings are paths, not general-purpose configuration channels.
		// Requiring an absolute, non-credential-shaped value prevents a variable
		// with a misleading trusted name from becoming a secret-smuggling route.
		return filepath.IsAbs(value) && RedactSubprocessText(value) == value
	}
	if _, isProxy := proxySubprocessEnv[name]; !isProxy {
		return true
	}
	// Proxy settings are useful in enterprise environments, but URL userinfo,
	// token query parameters, and other credential-shaped material must not be
	// copied into a model-controlled child process. Dropping the whole binding
	// is safer than silently changing how an authenticated proxy is reached.
	for _, candidate := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '|' }) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == "direct" || candidate == "off" {
			continue
		}
		// net/http accepts proxy values without an explicit scheme and treats
		// them as HTTP URLs. Parse those with the same interpretation; otherwise
		// "alice:secret@proxy:8080" looks like an opaque URL with scheme "alice"
		// and its userinfo would escape detection.
		parseTarget := candidate
		if !strings.Contains(parseTarget, "://") {
			parseTarget = "http://" + parseTarget
		}
		parsed, err := url.Parse(parseTarget)
		if err != nil {
			return false
		}
		if parsed.User != nil {
			return false
		}
		for key := range parsed.Query() {
			if isCredentialFieldName(key) {
				return false
			}
		}
	}
	return RedactSubprocessText(value) == value
}

func replaceSubprocessEnv(env []string, name, binding string) []string {
	canonicalName := canonicalSubprocessEnvName(name)
	for i, kv := range env {
		existingName, _, ok := strings.Cut(kv, "=")
		if ok && canonicalSubprocessEnvName(existingName) == canonicalName {
			env[i] = binding
			return env
		}
	}
	return append(env, binding)
}

func canonicalSubprocessEnvName(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// RedactSubprocessText removes credentials from child stdout/stderr and error
// strings before those strings can enter a tool result or hook response. It
// extends the high-confidence token scanner with generic secret assignments
// and URL userinfo while preserving JSON quoting for hook responses.
func RedactSubprocessText(text string) string {
	if text == "" {
		return text
	}
	text = Redact(text)
	if !mayContainGenericCredentialSyntax(text) {
		return text
	}
	text = escapedJSONAssignmentRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := escapedJSONAssignmentRE.FindStringSubmatch(match)
		if len(parts) < 5 || !isCredentialFieldName(parts[2]) {
			return match
		}
		return parts[1] + "[REDACTED]" + parts[4]
	})
	text = jsonAssignmentRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := jsonAssignmentRE.FindStringSubmatch(match)
		if len(parts) < 3 || !isCredentialFieldName(parts[2]) {
			return match
		}
		return parts[1] + `"[REDACTED]"`
	})
	text = quotedPlainAssignmentRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := quotedPlainAssignmentRE.FindStringSubmatch(match)
		if len(parts) < 3 || !isCredentialFieldName(parts[2]) {
			return match
		}
		value := match[len(parts[1]):]
		if value != "" && (value[0] == '"' || value[0] == '\'') {
			return parts[1] + string(value[0]) + "[REDACTED]" + string(value[0])
		}
		return parts[1] + "[REDACTED]"
	})
	text = authorizationHeaderRE.ReplaceAllString(text, `${1}${2} [REDACTED]`)
	text = databaseNetworkUserinfoRE.ReplaceAllString(text, `${1}[REDACTED]@${2}(`)
	text = plainAssignmentRE.ReplaceAllStringFunc(text, func(match string) string {
		parts := plainAssignmentRE.FindStringSubmatch(match)
		if len(parts) < 3 || !isCredentialFieldName(parts[1]) {
			return match
		}
		if strings.EqualFold(parts[1], "authorization") || strings.EqualFold(parts[1], "proxy-authorization") {
			// The dedicated header expression above preserves the auth scheme and
			// removes the complete payload rather than only its first word.
			return match
		}
		return parts[1] + parts[2] + "[REDACTED]"
	})
	return urlUserinfoRE.ReplaceAllString(text, `${1}[REDACTED]@`)
}

func mayContainGenericCredentialSyntax(text string) bool {
	if strings.Contains(text, "://") || strings.Contains(text, "@tcp(") || strings.Contains(text, "@unix(") {
		return true
	}
	data := []byte(text)
	for i := 0; i < len(data); {
		if !isASCIIIdentifierStart(data[i]) {
			i++
			continue
		}
		start := i
		i++
		for i < len(data) && isASCIIIdentifierContinue(data[i]) {
			i++
		}
		name := data[start:i]
		if !credentialNameMayMatch(name) || !isCredentialFieldNameBytes(name) {
			continue
		}
		pos := i
		if pos < len(data) && (data[pos] == '"' || data[pos] == '\'') {
			pos++
		} else if pos+1 < len(data) && data[pos] == '\\' && data[pos+1] == '"' {
			pos += 2
		}
		pos, _ = skipCredentialWhitespace(data, pos)
		if pos < len(data) && (data[pos] == ':' || data[pos] == '=') {
			return true
		}
	}
	return false
}

// RedactSubprocessTextWithEnv additionally removes the exact values of
// credential-shaped environment entries. It is intended for trusted,
// user-configured hooks that must retain their historical environment while
// keeping any accidentally echoed credentials out of model-visible output.
func RedactSubprocessTextWithEnv(text string, env []string) string {
	text = RedactSubprocessText(text)
	values := make([]string, 0)
	for _, binding := range env {
		name, value, ok := strings.Cut(binding, "=")
		if !ok || len(value) < 4 || !isCredentialFieldName(name) {
			continue
		}
		values = append(values, value)
	}
	// Longer overlapping credentials must be removed before their prefixes.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		text = strings.ReplaceAll(text, value, "[REDACTED]")
	}
	return text
}

func isCredentialFieldName(name string) bool {
	return isCredentialFieldNameBytes([]byte(name))
}

// IsCredentialFieldName reports whether a structured object key denotes a
// credential-bearing value. Callers should redact the complete value/subtree,
// not merely scan string contents: arbitrary passwords such as "hunter2" have
// no distinctive token shape. Matching normalizes case plus '-', '.', and '_'
// separators and understands common camelCase suffixes.
func IsCredentialFieldName(name string) bool {
	if isCredentialFieldName(name) {
		return true
	}
	// Structured tool arguments can safely treat an auth-labelled subtree as
	// sensitive. Keep this broader rule out of the subprocess text scanner:
	// diagnostic prose such as "auth=required" is status, not a credential.
	normalized := make([]byte, len(name))
	for i := range name {
		normalized[i] = normalizedCredentialNameByte(name[i])
	}
	n := string(normalized)
	if n == "auth" || n == "authentication" || n == "oauth" || n == "oauth2" {
		return true
	}
	for _, suffix := range []string{"_auth", "_authentication", "_oauth"} {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	for _, suffix := range []string{"Auth", "Authentication", "OAuth"} {
		if credentialNameHasCamelSuffix([]byte(name), suffix) {
			return true
		}
	}
	return false
}
