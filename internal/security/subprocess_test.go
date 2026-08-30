package security

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func TestRestrictedSubprocessEnvKeepsEssentialsAndDropsSecrets(t *testing.T) {
	in := []string{
		"PATH=/usr/local/bin:/usr/bin",
		"HOME=/Users/tester",
		"TMPDIR=/tmp/user",
		"LANG=zh_CN.UTF-8",
		"LC_ALL=zh_CN.UTF-8",
		"TZ=Asia/Shanghai",
		"GOPATH=/Users/tester/go",
		"PYTHONPATH=/workspace/lib",
		"OPENAI_API_KEY=sk-proj-should-never-reach-child",
		"TAVILY_TOKEN=tool-secret",
		"AWS_PROFILE=production",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"RANDOM_APPLICATION_SETTING=not-needed",
	}

	got := strings.Join(RestrictedSubprocessEnv(in, "METIS_HOOK_EVENT=PreToolUse"), "\n")
	for _, want := range []string{
		"PATH=/usr/local/bin:/usr/bin",
		"HOME=/Users/tester",
		"TMPDIR=/tmp/user",
		"LANG=zh_CN.UTF-8",
		"LC_ALL=zh_CN.UTF-8",
		"TZ=Asia/Shanghai",
		"GOPATH=/Users/tester/go",
		"PYTHONPATH=/workspace/lib",
		"METIS_HOOK_EVENT=PreToolUse",
		"AGENT=metis",
		"AI_AGENT=metis",
		"METIS=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("restricted env missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"OPENAI_API_KEY",
		"TAVILY_TOKEN",
		"AWS_PROFILE",
		"SSH_AUTH_SOCK",
		"RANDOM_APPLICATION_SETTING",
		"should-never-reach-child",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("restricted env retained %q:\n%s", forbidden, got)
		}
	}
}

func TestRestrictedSubprocessEnvKeepsSafeToolchainPathsOnly(t *testing.T) {
	in := []string{
		"CARGO_HOME=/Users/tester/.cargo",
		"RUSTUP_HOME=/Users/tester/.rustup",
		"PYENV_ROOT=/Users/tester/.pyenv",
		"ASDF_DIR=/opt/asdf",
		"ASDF_DATA_DIR=/Users/tester/.asdf",
		"MISE_DATA_DIR=/Users/tester/.local/share/mise",
		"MISE_CACHE_DIR=/Users/tester/.cache/mise",
		"MISE_CONFIG_DIR=/Users/tester/.config/mise",
		"MISE_STATE_DIR=/Users/tester/.local/state/mise",
		"NPM_CONFIG_PREFIX=/Users/tester/.local/npm",
		"NPM_TOKEN=npm-secret",
		"CARGO_REGISTRIES_PRIVATE_TOKEN=cargo-secret",
		"RUSTUP_TOOLCHAIN_TOKEN=rustup-secret",
		"NPM_CONFIG_USERCONFIG=/Users/tester/.npmrc",
		"PYENV_ROOT=relative/secret-path",
	}

	got := strings.Join(RestrictedSubprocessEnv(in), "\n")
	for _, want := range []string{
		"CARGO_HOME=/Users/tester/.cargo",
		"RUSTUP_HOME=/Users/tester/.rustup",
		"PYENV_ROOT=/Users/tester/.pyenv",
		"ASDF_DIR=/opt/asdf",
		"ASDF_DATA_DIR=/Users/tester/.asdf",
		"MISE_DATA_DIR=/Users/tester/.local/share/mise",
		"MISE_CACHE_DIR=/Users/tester/.cache/mise",
		"MISE_CONFIG_DIR=/Users/tester/.config/mise",
		"MISE_STATE_DIR=/Users/tester/.local/state/mise",
		"NPM_CONFIG_PREFIX=/Users/tester/.local/npm",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("restricted env missing safe toolchain path %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"NPM_TOKEN", "npm-secret",
		"CARGO_REGISTRIES_PRIVATE_TOKEN", "cargo-secret",
		"RUSTUP_TOOLCHAIN_TOKEN", "rustup-secret",
		"NPM_CONFIG_USERCONFIG", ".npmrc",
		"PYENV_ROOT=relative/secret-path",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("restricted env retained forbidden binding %q:\n%s", forbidden, got)
		}
	}
}

func TestRestrictedSubprocessEnvKeepsCredentialFreeProxyConfiguration(t *testing.T) {
	in := []string{
		"HTTP_PROXY=http://proxy.corp.example:8080",
		"https_proxy=https://proxy.corp.example:8443",
		"NO_PROXY=localhost,127.0.0.1,.corp.example",
		"GOPROXY=https://proxy.golang.org,direct",
		"GOPRIVATE=git.corp.example/*",
		"GONOPROXY=git.corp.example/*",
		"GONOSUMDB=git.corp.example/*",
		"HTTPS_PROXY=https://alice:super-secret@proxy.corp.example:8443",
		"ALL_PROXY=http://proxy.corp.example:8080?token=super-secret",
		"https_proxy=alice:scheme-less-secret@proxy.corp.example:8443",
	}

	got := strings.Join(RestrictedSubprocessEnv(in), "\n")
	for _, want := range []string{
		"HTTP_PROXY=http://proxy.corp.example:8080",
		"https_proxy=https://proxy.corp.example:8443",
		"NO_PROXY=localhost,127.0.0.1,.corp.example",
		"GOPROXY=https://proxy.golang.org,direct",
		"GOPRIVATE=git.corp.example/*",
		"GONOPROXY=git.corp.example/*",
		"GONOSUMDB=git.corp.example/*",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("restricted env missing safe proxy setting %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"HTTPS_PROXY=", "ALL_PROXY=", "alice", "super-secret", "scheme-less-secret"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("restricted env retained credential-bearing proxy value %q:\n%s", forbidden, got)
		}
	}
}

func TestRestrictedSubprocessEnvExplicitValuesReplaceParent(t *testing.T) {
	got := strings.Join(RestrictedSubprocessEnv(
		[]string{"PATH=/old", "LANG=C", "METIS_HOOK_EVENT=old"},
		"PATH=/new", "METIS_HOOK_EVENT=SessionStart",
	), "\n")
	if strings.Contains(got, "PATH=/old") || strings.Contains(got, "METIS_HOOK_EVENT=old") {
		t.Fatalf("explicit values did not replace parent:\n%s", got)
	}
	for _, want := range []string{"PATH=/new", "METIS_HOOK_EVENT=SessionStart"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing explicit value %q:\n%s", want, got)
		}
	}
}

func TestRestrictedSubprocessEnvUsesPlatformEnvNameSemantics(t *testing.T) {
	got := strings.Join(RestrictedSubprocessEnv(
		[]string{"PATH=/safe", "Path=/unexpected"},
	), "\n")
	if runtime.GOOS == "windows" {
		if strings.Contains(got, "PATH=/safe") || !strings.Contains(got, "Path=/unexpected") {
			t.Fatalf("Windows env names should be case-insensitive:\n%s", got)
		}
		return
	}
	if !strings.Contains(got, "PATH=/safe") || strings.Contains(got, "Path=/unexpected") {
		t.Fatalf("Unix env names should be case-sensitive:\n%s", got)
	}
}

func TestRedactSubprocessTextCoversKnownTokensAssignmentsAndURLUserinfo(t *testing.T) {
	githubToken := "ghp_abcdefghijklmnopqrstuvwxyz1234567890"
	in := strings.Join([]string{
		"known=" + githubToken,
		"CUSTOM_API_KEY=plain-looking-secret",
		`{"password":"hunter2","message":"keep me"}`,
		"request failed: https://alice:s3cr3t@example.test/private",
	}, "\n")

	got := RedactSubprocessText(in)
	for _, forbidden := range []string{githubToken, "plain-looking-secret", "hunter2", "s3cr3t"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("subprocess text leaked %q: %s", forbidden, got)
		}
	}
	for _, want := range []string{"CUSTOM_API_KEY=[REDACTED]", `"password":"[REDACTED]"`, "https://alice:[REDACTED]@example.test", "keep me"} {
		if !strings.Contains(got, want) {
			t.Fatalf("redacted text missing %q: %s", want, got)
		}
	}
}

func TestRedactSubprocessTextCoversAuthorizationDatabaseDSNsAndEscapedJSON(t *testing.T) {
	in := strings.Join([]string{
		"Authorization: Basic dXNlcjpzdXBlci1zZWNyZXQ=",
		"Proxy-Authorization: Bearer opaque-proxy-token",
		"postgresql://alice:db-password@db.example:5432/app",
		"mongodb+srv://bob:mongo-password@cluster.example/app",
		"alice:mysql-password@tcp(db.example:3306)/app",
		`payload={\"client_secret\":\"escaped-secret-value\",\"message\":\"keep me\"}`,
	}, "\n")

	got := RedactSubprocessText(in)
	for _, forbidden := range []string{
		"dXNlcjpzdXBlci1zZWNyZXQ=", "opaque-proxy-token", "db-password",
		"mongo-password", "mysql-password", "escaped-secret-value",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("subprocess text leaked %q: %s", forbidden, got)
		}
	}
	for _, want := range []string{
		"Authorization: Basic [REDACTED]",
		"Proxy-Authorization: Bearer [REDACTED]",
		"postgresql://alice:[REDACTED]@db.example",
		"mongodb+srv://bob:[REDACTED]@cluster.example",
		"alice:[REDACTED]@tcp(db.example:3306)",
		`{\"client_secret\":\"[REDACTED]\",\"message\":\"keep me\"}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("redacted text missing %q: %s", want, got)
		}
	}
}

func TestRedactSubprocessTextPreservesOrdinaryMetadataAndValidJSON(t *testing.T) {
	in := `{"decision":"deny","reason":"auth=required","author":"Alice","auth_status":"required","token_count":42}`
	got := RedactSubprocessText(in)
	if got != in {
		t.Fatalf("ordinary metadata changed:\n got: %s\nwant: %s", got, in)
	}
}

func TestRedactSubprocessTextWithEnvRemovesExactTrustedHookCredential(t *testing.T) {
	const secret = "opaque-enterprise-hook-value"
	got := RedactSubprocessTextWithEnv(
		`{"decision":"deny","reason":"upstream returned `+secret+`"}`,
		[]string{"OPA_TOKEN=" + secret, "AUTHOR=Alice"},
	)
	if strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("exact environment credential was not redacted: %s", got)
	}
	if !strings.Contains(got, `"decision":"deny"`) {
		t.Fatalf("redaction broke hook JSON: %s", got)
	}
}

func TestRedactSubprocessTextHandlesEscapedQuotesWithoutLeakingSuffixes(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		forbidden string
		want      string
	}{
		{
			name:      "escaped JSON representation",
			input:     `payload={\"password\":\"foo\\\"escaped-secret-suffix\",\"message\":\"keep me\"}`,
			forbidden: "escaped-secret-suffix",
			want:      `payload={\"password\":\"[REDACTED]\",\"message\":\"keep me\"}`,
		},
		{
			name:      "ordinary JSON",
			input:     `{"password":"foo\"json-secret-suffix","message":"keep me"}`,
			forbidden: "json-secret-suffix",
			want:      `{"password":"[REDACTED]","message":"keep me"}`,
		},
		{
			name:      "plain quoted assignment",
			input:     `password="foo\"plain-secret-suffix" message=keep`,
			forbidden: "plain-secret-suffix",
			want:      `password=[REDACTED] message=keep`,
		},
		{
			name:      "OAuth JSON layer",
			input:     `{"access_token":"foo\"oauth-secret-suffix","message":"keep me"}`,
			forbidden: "oauth-secret-suffix",
			want:      `{"access_token":"[REDACTED]","message":"keep me"}`,
		},
		{
			name:      "quoted YAML key raw value",
			input:     `"password": hunter2`,
			forbidden: "hunter2",
			want:      `"password": [REDACTED]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactSubprocessText(tt.input)
			if strings.Contains(got, tt.forbidden) || got != tt.want {
				t.Fatalf("RedactSubprocessText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFileCredentialRedactorPreservesEscapedJSONSiblingField(t *testing.T) {
	source := "payload={\\\"password\\\":\n\\\"foo\\\\\\\"escaped-secret-suffix\\\",\\\"message\\\":\\\"keep me\\\"}\n"
	redactor := NewFileCredentialRedactor([]byte(source))
	lineStart, line := sourceTestLine(t, source, 2)
	got := redactor.RedactLineAt(lineStart, line)
	want := `\"[REDACTED]\",\"message\":\"keep me\"}`
	if got != want {
		t.Fatalf("RedactLineAt() = %q, want %q", got, want)
	}
}

func TestFileCredentialRedactorPreservesYAMLCommentsAndSiblingLines(t *testing.T) {
	source := "password:\n  # populated later\n\n  real-secret\nordinary: keep\n"
	redactor := NewFileCredentialRedactor([]byte(source))
	for _, lineNumber := range []int{2, 3, 5} {
		lineStart, line := sourceTestLine(t, source, lineNumber)
		if got := redactor.RedactLineAt(lineStart, line); got != line {
			t.Fatalf("non-value line %d changed: got %q, want %q", lineNumber, got, line)
		}
	}
	lineStart, line := sourceTestLine(t, source, 4)
	if got := redactor.RedactLineAt(lineStart, line); strings.Contains(got, "real-secret") {
		t.Fatalf("credential after comments was not redacted: %q", got)
	}
}

func TestFileCredentialRedactorRedactsMultilineCredentialFragments(t *testing.T) {
	privateKeyBody := strings.Repeat("M", 80)
	tests := []struct {
		name   string
		source string
		line   int
		secret string
	}{
		{
			name: "PEM body without markers",
			source: "-----BEGIN PRIVATE KEY-----\n" + privateKeyBody +
				"\n-----END PRIVATE KEY-----\n",
			line:   2,
			secret: privateKeyBody,
		},
		{
			name: "indented CRLF PEM body",
			source: "    -----BEGIN PRIVATE KEY-----\r\n" + privateKeyBody +
				"\r\n    -----END PRIVATE KEY-----\r\n",
			line:   2,
			secret: privateKeyBody,
		},
		{
			name: "BOM PEM body",
			source: "\ufeff-----BEGIN PRIVATE KEY-----\n" + privateKeyBody +
				"\n-----END PRIVATE KEY-----\n",
			line:   2,
			secret: privateKeyBody,
		},
		{
			name:   "truncated PEM body",
			source: "-----BEGIN PRIVATE KEY-----\n" + privateKeyBody + "\n",
			line:   2,
			secret: privateKeyBody,
		},
		{
			name:   "JSON OAuth value",
			source: "{\n  \"access_token\":\n  \"oauth-secret-value\"\n}\n",
			line:   3,
			secret: "oauth-secret-value",
		},
		{
			name:   "generic JSON password",
			source: "{\n  \"password\":\n  \"database-password\"\n}\n",
			line:   3,
			secret: "database-password",
		},
		{
			name:   "camelCase JSON client secret",
			source: "{\n  \"clientSecret\":\n  \"camel-json-secret\"\n}\n",
			line:   3,
			secret: "camel-json-secret",
		},
		{
			name:   "double-quoted YAML key with raw value",
			source: "\"password\":\n  quoted-key-secret\n",
			line:   2,
			secret: "quoted-key-secret",
		},
		{
			name:   "single-quoted YAML key with raw value",
			source: "'password':\n  single-quoted-key-secret\n",
			line:   2,
			secret: "single-quoted-key-secret",
		},
		{
			name:   "escaped JSON client secret",
			source: "payload={\\\"client_secret\\\":\n  \\\"escaped-secret-value\\\"}\n",
			line:   2,
			secret: "escaped-secret-value",
		},
		{
			name:   "escaped quote in escaped JSON value",
			source: "payload={\\\"client_secret\\\":\n\\\"foo\\\\\\\"escaped-secret-suffix\\\"}\n",
			line:   2,
			secret: "escaped-secret-suffix",
		},
		{
			name:   "plain assignment",
			source: "CUSTOM_API_KEY=\nplain-assignment-secret\n",
			line:   2,
			secret: "plain-assignment-secret",
		},
		{
			name:   "TOML triple-quoted value",
			source: "password = \"\"\"\ntriple-quoted-secret\n\"\"\"\n",
			line:   2,
			secret: "triple-quoted-secret",
		},
		{
			name:   "YAML physical multiline quoted value",
			source: "password: \"first-secret-part\nsecond-secret-part\"\n",
			line:   2,
			secret: "second-secret-part",
		},
		{
			name:   "YAML password",
			source: "password:\n  yaml-password-value\n",
			line:   2,
			secret: "yaml-password-value",
		},
		{
			name:   "unquoted value with spaces",
			source: "password:\n  my secret password\n",
			line:   2,
			secret: "my secret password",
		},
		{
			name:   "YAML list item value",
			source: "password:\n  - actual-list-secret\n",
			line:   2,
			secret: "- actual-list-secret",
		},
		{
			name:   "YAML block scalar",
			source: "password: |\n  yaml-block-secret\n  second-secret-line\nordinary: keep\n",
			line:   2,
			secret: "yaml-block-secret",
		},
		{
			name:   "YAML block scalar after blank line",
			source: "password: |\n  first-secret-line\n\n  second-secret-line\nordinary: keep\n",
			line:   4,
			secret: "second-secret-line",
		},
		{
			name:   "credential after YAML comment",
			source: "password:\n  # populated by deploy\n  secret-after-comment\n",
			line:   3,
			secret: "secret-after-comment",
		},
		{
			name:   "authorization header",
			source: "Authorization: Bearer\nauth-payload-value\n",
			line:   2,
			secret: "auth-payload-value",
		},
		{
			name:   "bare bearer across lines",
			source: "Bearer\n" + strings.Repeat("Z", 24) + "\n",
			line:   2,
			secret: strings.Repeat("Z", 24),
		},
		{
			name:   "short opaque bearer punctuation",
			source: "Bearer\nabc+def/ghi~\n",
			line:   2,
			secret: "abc+def/ghi~",
		},
		{
			name:   "bare bearer nested in ordinary assignment",
			source: "foo:\nBearer\n" + strings.Repeat("Y", 24) + "\n",
			line:   3,
			secret: strings.Repeat("Y", 24),
		},
		{
			name:   "credential nested in ordinary YAML",
			source: "metadata:\n  password:\n    nested-password-value\n",
			line:   3,
			secret: "nested-password-value",
		},
		{
			name:   "bare bearer nested in credential value",
			source: "password:\nBearer\nTOKEN\n",
			line:   3,
			secret: "TOKEN",
		},
		{
			name:   "bare bearer nested in quoted credential key",
			source: "\"password\":\nBearer\nSHORT\n",
			line:   3,
			secret: "SHORT",
		},
		{
			name:   "credential nested below quoted outer key",
			source: "\"credentials\":\n  password:\n    deeply-nested-secret\n",
			line:   3,
			secret: "deeply-nested-secret",
		},
		{
			name:   "escaped quote in JSON value",
			source: "{\n\"password\":\n\"foo\\\"bar-secret-suffix\"\n}\n",
			line:   3,
			secret: "bar-secret-suffix",
		},
		{
			name:   "escaped quote in plain value",
			source: "password=\n\"foo\\\"plain-secret-suffix\"\n",
			line:   2,
			secret: "plain-secret-suffix",
		},
		{
			name:   "CRLF JSON value",
			source: "{\r\n  \"access_token\":\r\n  \"crlf-secret-value\"\r\n}\r\n",
			line:   3,
			secret: "crlf-secret-value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redactor := NewFileCredentialRedactor([]byte(tt.source))
			lineStart, line := sourceTestLine(t, tt.source, tt.line)
			got := redactor.RedactLineAt(lineStart, line)
			if strings.Contains(got, tt.secret) || !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("RedactLine() = %q, want credential value redacted", got)
			}
		})
	}
}

func TestFileCredentialRedactorUsesSourceLineAndColumn(t *testing.T) {
	const secret = "short"
	source := "password:\n" + secret + "\nordinary=" + secret + "\n"
	redactor := NewFileCredentialRedactor([]byte(source))

	if got := redactor.RedactLineAt(len("password:\n"), secret); got != "[REDACTED]" {
		t.Fatalf("credential line = %q, want [REDACTED]", got)
	}
	if got := redactor.RedactLineAt(len("password:\n"+secret+"\n"), "ordinary="+secret); got != "ordinary="+secret {
		t.Fatalf("unrelated line changed: %q", got)
	}
}

func TestFileCredentialRedactorPreservesPlaceholdersAndSingleLineExamples(t *testing.T) {
	githubToken := "ghp_" + strings.Repeat("A", 36)
	stripeToken := "sk_test_" + strings.Repeat("B", 24)
	bearerToken := strings.Repeat("C", 24)
	tests := []struct {
		name   string
		source string
	}{
		{name: "empty OAuth value", source: "{\n  \"access_token\":\n  \"\"\n}\n"},
		{name: "environment placeholder", source: "CUSTOM_API_KEY=\n${CUSTOM_API_KEY}\n"},
		{name: "single-line GitHub token", source: "token=" + githubToken + "\n"},
		{name: "single-line Stripe token", source: "stripe=" + stripeToken + "\n"},
		{name: "single-line bearer", source: "Authorization: Bearer " + bearerToken + "\n"},
		{name: "single-line OAuth JSON", source: "{\"access_token\":\"ordinary-example\"}\n"},
		{
			name: "PEM markers in source code",
			source: "const begin = \"-----BEGIN PRIVATE KEY-----\"\n" +
				"const pemRE = `-----BEGIN[ A-Z]+PRIVATE KEY-----`\n" +
				"const end = \"-----END PRIVATE KEY-----\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redactor := NewFileCredentialRedactor([]byte(tt.source))
			for i, line := range strings.Split(strings.TrimSuffix(tt.source, "\n"), "\n") {
				lineStart, _ := sourceTestLine(t, tt.source, i+1)
				if got := redactor.RedactLineAt(lineStart, line); got != line {
					t.Fatalf("line %d changed: got %q, want %q", i+1, got, line)
				}
			}
		})
	}

	// The source-aware pass intentionally ignores complete single-line values;
	// the existing output-boundary redactor remains responsible for them.
	for _, output := range []struct {
		text   string
		secret string
	}{
		{text: "token=" + githubToken, secret: githubToken},
		{text: "stripe=" + stripeToken, secret: stripeToken},
		{text: "Authorization: Bearer " + bearerToken, secret: bearerToken},
	} {
		if got := RedactSubprocessText(output.text); strings.Contains(got, output.secret) {
			t.Fatalf("final output redactor did not remove %q", output.secret)
		}
	}
}

func TestFileCredentialRedactorRedactsLiteralExampleValueWithoutHidingFile(t *testing.T) {
	source := "ordinary: keep\naccess_token:\n  example-token\nafter: visible\n"
	redactor := NewFileCredentialRedactor([]byte(source))
	for _, lineNumber := range []int{1, 4} {
		lineStart, line := sourceTestLine(t, source, lineNumber)
		if got := redactor.RedactLineAt(lineStart, line); got != line {
			t.Fatalf("ordinary line %d changed: %q", lineNumber, got)
		}
	}
	lineStart, line := sourceTestLine(t, source, 3)
	if got := redactor.RedactLineAt(lineStart, line); strings.Contains(got, "example-token") {
		t.Fatalf("credential-key literal was not redacted: %q", got)
	}
}

func TestCredentialFieldNamesIncludeCommonCamelCaseForms(t *testing.T) {
	for _, name := range []string{
		"apiKey", "accessToken", "refreshToken", "idToken", "subjectToken",
		"clientSecret", "clientAssertion", "privateKey", "githubToken", "servicePassword",
	} {
		if !isCredentialFieldName(name) || !isCredentialFieldNameBytes([]byte(name)) || !IsCredentialFieldName(name) {
			t.Fatalf("camelCase credential field %q was not classified", name)
		}
	}
}

func TestStructuredCredentialFieldNamesIncludeAuthSubtrees(t *testing.T) {
	for _, name := range []string{"auth", "requestAuth", "authentication", "oauth", "serviceOAuth", "proxy-auth"} {
		if !IsCredentialFieldName(name) {
			t.Fatalf("structured auth field %q was not classified", name)
		}
	}
	for _, name := range []string{"author", "auth_status", "token_count", "passwordless", "credentialType"} {
		if IsCredentialFieldName(name) {
			t.Fatalf("ordinary structured field %q was misclassified", name)
		}
	}
	// The broader structured rule must not alter diagnostic text redaction.
	if got := RedactSubprocessText("auth=required auth_status=ready"); got != "auth=required auth_status=ready" {
		t.Fatalf("auth status text was over-redacted: %q", got)
	}
}

func TestFileCredentialRedactorKeepsPEMMetadataBounded(t *testing.T) {
	body := strings.Repeat("A\n", 100000)
	source := "-----BEGIN PRIVATE KEY-----\n" + body + "-----END PRIVATE KEY-----\n"
	redactor := NewFileCredentialRedactor([]byte(source))
	if !redactor.HasRedactions() || redactor.overflow {
		t.Fatalf("PEM redactor = %+v, want ordinary structural redaction", redactor)
	}
	if len(redactor.spans) != 1 {
		t.Fatalf("PEM stored %d spans, want one source range", len(redactor.spans))
	}
}

func TestFileCredentialRedactorSpanLimitFailsClosed(t *testing.T) {
	source := strings.Repeat("password:\nsecret-value\n", maxFileCredentialSpans+1)
	redactor := NewFileCredentialRedactor([]byte(source))
	if !redactor.overflow || !redactor.HasRedactions() {
		t.Fatalf("redactor = %+v, want fail-closed overflow state", redactor)
	}
	if got := redactor.RedactLineAt(0, "password:"); got != "[REDACTED]" {
		t.Fatalf("overflow RedactLineAt() = %q, want full-line redaction", got)
	}
}

func TestFileCredentialRedactorCandidateLimitFailsClosed(t *testing.T) {
	source := strings.Repeat("password:\n${TOKEN}\n", maxFileCredentialCandidates+1)
	redactor := NewFileCredentialRedactor([]byte(source))
	if !redactor.overflow || !redactor.HasRedactions() {
		t.Fatalf("redactor = %+v, want rejected-candidate flood to fail closed", redactor)
	}
}

func sourceTestLine(t *testing.T, source string, lineNumber int) (int, string) {
	t.Helper()
	start := 0
	for line := 1; line < lineNumber; line++ {
		rel := strings.IndexByte(source[start:], '\n')
		if rel < 0 {
			t.Fatalf("source has no line %d", lineNumber)
		}
		start += rel + 1
	}
	end := strings.IndexByte(source[start:], '\n')
	if end < 0 {
		end = len(source) - start
	}
	line := strings.TrimSuffix(source[start:start+end], "\r")
	return start, line
}

var benchmarkFileCredentialRedactor FileCredentialRedactor
var benchmarkRedactedSubprocessText string

func BenchmarkNewFileCredentialRedactorClean(b *testing.B) {
	data := bytes.Repeat([]byte("ordinary project source with public fields\n"), 140000)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkFileCredentialRedactor = NewFileCredentialRedactor(data)
	}
}

func BenchmarkNewFileCredentialRedactorWithMultilineValue(b *testing.B) {
	data := append(bytes.Repeat([]byte("ordinary project source\n"), 100000), []byte("password:\nreal-secret-value\n")...)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkFileCredentialRedactor = NewFileCredentialRedactor(data)
	}
}

func BenchmarkNewFileCredentialRedactorWithLeadingMultilineValue(b *testing.B) {
	data := append([]byte("password:\nreal-secret-value\n"), bytes.Repeat([]byte("ordinary project source\n"), 100000)...)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkFileCredentialRedactor = NewFileCredentialRedactor(data)
	}
}

func BenchmarkNewFileCredentialRedactorSourceMarkerWithCleanSuffix(b *testing.B) {
	data := append([]byte("const marker = \"-----BEGIN PRIVATE KEY-----\"\n"), bytes.Repeat([]byte("ordinary project source\n"), 100000)...)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkFileCredentialRedactor = NewFileCredentialRedactor(data)
	}
}

func BenchmarkRedactSubprocessTextCleanLarge(b *testing.B) {
	data := string(bytes.Repeat([]byte("  1234\tordinary project source with public fields\n"), 100000))
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkRedactedSubprocessText = RedactSubprocessText(data)
	}
}
