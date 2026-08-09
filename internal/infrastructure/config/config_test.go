// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package config

import (
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateDatabaseSettings_Success(t *testing.T) {
	cfg := &Config{
		PGHost:          "db.example.com",
		PGPort:          "5432",
		PGUser:          "campaign",
		PGDatabase:      "campaign",
		PGEngine:        "postgres",
		DatabaseURL:     "postgres://campaign:secret@db.example.com:5432/campaign",
		passwordPresent: true,
	}
	assert.NoError(t, cfg.ValidateDatabaseSettings())
}

func TestValidateDatabaseSettings_EmptyOptional(t *testing.T) {
	cfg := &Config{}
	assert.NoError(t, cfg.ValidateDatabaseSettings())
}

func TestValidateDatabaseSettings_ExplicitDatabaseURL(t *testing.T) {
	cfg := &Config{DatabaseURL: "postgres://campaign:secret@db.example.com:5432/campaign"}
	assert.NoError(t, cfg.ValidateDatabaseSettings())
}

func TestValidateDatabaseSettings_MissingFields(t *testing.T) {
	cfg := &Config{PGHost: "localhost"}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PGUSER")
	assert.Contains(t, err.Error(), "PGDATABASE")
	assert.NotContains(t, err.Error(), "secret")
}

func TestValidateDatabaseSettings_UnsupportedEngine(t *testing.T) {
	cfg := &Config{
		PGHost:          "db.example.com",
		PGPort:          "5432",
		PGUser:          "campaign",
		PGDatabase:      "campaign",
		PGEngine:        "mysql",
		DatabaseURL:     "postgres://campaign:secret@db.example.com:5432/campaign",
		passwordPresent: true,
	}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported database engine")
}

func TestLoadDatabaseFromEnv_ComposesURL(t *testing.T) {
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGPORT", "5433")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", "s3cret-value")
	t.Setenv("PGDATABASE", "campaign")
	t.Setenv("PGENGINE", "postgresql")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()

	require.NoError(t, cfg.ValidateDatabaseSettings())
	assert.Equal(t, "localhost", cfg.PGHost)
	assert.Equal(t, "5433", cfg.PGPort)
	assert.Equal(t, "app", cfg.PGUser)
	assert.Equal(t, "campaign", cfg.PGDatabase)

	u, err := url.Parse(cfg.DatabaseURL)
	require.NoError(t, err)
	assert.Equal(t, "postgres", u.Scheme)
	assert.Equal(t, "app", u.User.Username())
	pass, ok := u.User.Password()
	require.True(t, ok)
	assert.Equal(t, "s3cret-value", pass)
	assert.Equal(t, "localhost:5433", u.Host)
	assert.Equal(t, "/campaign", u.Path)

	redacted := cfg.RedactedDatabaseHost()
	assert.Equal(t, "localhost:5433/campaign", redacted)
	assert.NotContains(t, redacted, "s3cret")
}

func TestLoadDatabaseFromEnv_PasswordSpecialCharacters(t *testing.T) {
	const password = "p@ss:w/ord"
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGPORT", "5432")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", password)
	t.Setenv("PGDATABASE", "campaign")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()

	require.NoError(t, cfg.ValidateDatabaseSettings())
	u, err := url.Parse(cfg.DatabaseURL)
	require.NoError(t, err)
	pass, ok := u.User.Password()
	require.True(t, ok)
	assert.Equal(t, password, pass)
	assert.Contains(t, cfg.DatabaseURL, "p%40ss%3Aw%2Ford")
	assert.NotContains(t, cfg.RedactedDatabaseHost(), password)
}

func TestValidateDatabaseSettings_CompleteFieldsMissingURL(t *testing.T) {
	cfg := &Config{
		PGHost:          "localhost",
		PGPort:          "5432",
		PGUser:          "app",
		PGDatabase:      "campaign",
		passwordPresent: true,
		// DatabaseURL intentionally unset — simulates hand-built Config
		// without loadDatabaseFromEnv.
	}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DatabaseURL is empty")
	assert.Contains(t, err.Error(), "loadDatabaseFromEnv")
}

func TestValidateDatabaseSettings_DatabaseURLWithPartialPGRequiresPassword(t *testing.T) {
	// Explicit DATABASE_URL must not mask a partial PG* set (FR-009).
	cfg := &Config{
		PGHost:      "localhost",
		PGUser:      "app",
		PGDatabase:  "campaign",
		DatabaseURL: "postgres://app:secret@localhost:5432/campaign",
	}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PGPASSWORD")
}

func TestLoadDatabaseFromEnv_IPv6Host(t *testing.T) {
	t.Setenv("PGHOST", "2001:db8::1")
	t.Setenv("PGPORT", "5432")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", "x")
	t.Setenv("PGDATABASE", "campaign")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()

	u, err := url.Parse(cfg.DatabaseURL)
	require.NoError(t, err)
	assert.Equal(t, "[2001:db8::1]:5432", u.Host)
	assert.Equal(t, "[2001:db8::1]:5432/campaign", cfg.RedactedDatabaseHost())
}

func TestLoadDatabaseFromEnv_IncompleteSkipsURL(t *testing.T) {
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGDATABASE", "campaign")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()
	assert.Empty(t, cfg.DatabaseURL)
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cret")
}

func TestValidateDatabaseSettings_PartialEngineOnly(t *testing.T) {
	cfg := &Config{PGEngine: "postgres"}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PGHOST")
}

func TestValidateDatabaseSettings_PartialPortOnly(t *testing.T) {
	cfg := &Config{PGPort: "5432", pgPortPresent: true}
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PGHOST")
}

func TestLoadDatabaseFromEnv_EngineOnlyFailsValidation(t *testing.T) {
	t.Setenv("PGHOST", "")
	t.Setenv("PGPORT", "")
	t.Setenv("PGUSER", "")
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGDATABASE", "")
	t.Setenv("PGENGINE", "postgres")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required database settings")
}

func TestLoadDatabaseFromEnv_PortOnlyFailsValidation(t *testing.T) {
	t.Setenv("PGHOST", "")
	t.Setenv("PGPORT", "5432")
	t.Setenv("PGUSER", "")
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGDATABASE", "")
	t.Setenv("PGENGINE", "")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()
	assert.True(t, cfg.pgPortPresent)
	err := cfg.ValidateDatabaseSettings()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required database settings")
}

func TestLoadDatabaseFromEnv_DefaultPort(t *testing.T) {
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGPORT", "")
	t.Setenv("PGUSER", "app")
	t.Setenv("PGPASSWORD", "x")
	t.Setenv("PGDATABASE", "campaign")

	cfg := &Config{}
	cfg.loadDatabaseFromEnv()
	assert.Equal(t, "5432", cfg.PGPort)
}

func TestConfigString_RedactsSecrets(t *testing.T) {
	cfg := &Config{
		Host:                    "*",
		Port:                    "8080",
		DatabaseURL:             "postgres://campaign:s3cret-value@db.example.com:5432/campaign", // secretlint-disable-line -- intentional fake DSN
		CredentialEncryptionKey: "TEZYLWNhbXBhaWduLWxvY2FsLWRldi1hZXMtMjU2ISE=",
		// Every secret-bearing field belongs in this fixture, not just the ones that
		// existed when it was written: the contract this test pins is "no configured
		// secret reaches a log", and a field added without a line here is a contract
		// nobody is checking. AIProxyURL/AIModel are populated too, so the assertions
		// below can show they are deliberately NOT masked — a masked URL would make
		// "is the proxy wired?" undiagnosable from a log.
		AIProxyURL:      "https://litellm.example.com",
		AIAPIKey:        "sk-ai-s3cret-value", // secretlint-disable-line -- intentional fake key
		AIModel:         "us.anthropic.claude-sonnet-4-20250514-v1:0",
		PGHost:          "db.example.com",
		PGUser:          "campaign",
		PGDatabase:      "campaign",
		passwordPresent: true,
	}

	for _, formatted := range []string{
		cfg.String(),
		cfg.GoString(),
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%#v", cfg),
	} {
		assert.NotContains(t, formatted, "s3cret-value")
		assert.NotContains(t, formatted, "TEZYLWNhbXBhaWduLWxvY2FsLWRldi1hZXMtMjU2ISE=")
		assert.Contains(t, formatted, "[redacted]")
		assert.Contains(t, formatted, "xxxxx")
		assert.Contains(t, formatted, "db.example.com")
		// The one thing this string is for is telling an operator whether copy
		// generation is configured, and `https://xxxxx` still says so — plus whether
		// the hop is TLS. The host itself is operator input and can BE the secret
		// (`AI_PROXY_URL=https://sup3r-s3cret/` is a well-formed URL), so it is masked.
		assert.Contains(t, formatted, `AIProxyURL:"https://xxxxx"`)
		assert.NotContains(t, formatted, "litellm.example.com")
		// A model id is not a credential, and an operator diagnosing bad copy needs it.
		assert.Contains(t, formatted, "us.anthropic.claude-sonnet-4-20250514-v1:0")
	}
}

func TestConfigString_RedactsKeywordAndMalformedDSN(t *testing.T) {
	cases := []string{
		"host=localhost user=app password=s3cret-value dbname=campaign",
		"://not-a-valid-url password=s3cret-value",
		"postgres://campaign@db.example.com:5432/campaign?password=s3cret-value",
	}
	for _, dsn := range cases {
		cfg := &Config{DatabaseURL: dsn}
		formatted := cfg.String()
		assert.NotContains(t, formatted, "s3cret-value", "dsn=%q", dsn)
		assert.Contains(t, formatted, "[redacted]", "dsn=%q", dsn)
	}
}

// TestEnvOrDefaultUnlessSet_DistinguishesUnsetFromEmpty pins the ONLY behavioural
// difference from envOrDefault: an explicitly-empty value survives instead of
// falling through to the default. This is what makes NATS_URL="" a usable
// disable switch for index publishing.
func TestEnvOrDefaultUnlessSet_DistinguishesUnsetFromEmpty(t *testing.T) {
	const key = "LFX_TEST_UNSET_VS_EMPTY"

	t.Run("unset falls back to the default", func(t *testing.T) {
		require.NoError(t, os.Unsetenv(key))
		if got := envOrDefaultUnlessSet(key, "fallback"); got != "fallback" {
			t.Fatalf("unset: got %q, want %q", got, "fallback")
		}
	})

	t.Run("explicitly empty stays empty", func(t *testing.T) {
		t.Setenv(key, "")
		// envOrDefault would return "fallback" here — that difference is the point.
		if got := envOrDefaultUnlessSet(key, "fallback"); got != "" {
			t.Fatalf("explicit empty: got %q, want %q (the disable switch is unreachable)", got, "")
		}
	})

	t.Run("set wins over the default", func(t *testing.T) {
		t.Setenv(key, "nats://custom:4222")
		if got := envOrDefaultUnlessSet(key, "fallback"); got != "nats://custom:4222" {
			t.Fatalf("set: got %q, want %q", got, "nats://custom:4222")
		}
	})
}

// TestConfigString_RedactsNATSCredentials pins that String() does not leak the broker password.
// A NATS URL may carry userinfo (nats://user:pass@host:4222), String() promises a log-safe
// representation, and anything that logs the config would otherwise put that password in the
// pod logs. The host is deliberately KEPT — it is what makes an indexing outage diagnosable.
func TestConfigString_RedactsNATSCredentials(t *testing.T) {
	cfg := &Config{NATSUrl: "nats://svcuser:sup3r-s3cret@nats.lfx.svc:4222"} // secretlint-disable-line -- fixture asserting the password is redacted

	for _, formatted := range []string{cfg.String(), cfg.GoString(), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg)} {
		assert.NotContains(t, formatted, "sup3r-s3cret", "the broker password must never reach a log line")
		assert.NotContains(t, formatted, "svcuser", "the broker username is part of the credential")
		// The host must survive: redacting wholesale would make an outage undiagnosable.
		assert.Contains(t, formatted, "nats.lfx.svc:4222")
	}
}

// TestConfigString_RedactsAIProxyCredentials pins that String() does not leak a credential
// carried INSIDE the proxy URL. The field looks secret-free — the key has its own field —
// but that is a property of what an operator typed, not of the field, and a URL has several
// places a token rides for free. Userinfo and the query were the obvious two; the PATH is
// here because it is the one that survived two earlier rounds of this exact fix, on the
// reasoning that url.Parse had already split the dangerous components out. Where the
// delimiters fall says nothing about what a component holds.
func TestConfigString_RedactsAIProxyCredentials(t *testing.T) {
	for name, raw := range map[string]string{
		"userinfo": "https://svcuser:sup3r-s3cret@litellm.example.com/v1",          // secretlint-disable-line -- fixture
		"query":    "https://litellm.example.com/v1?api-key=sup3r-s3cret",          // secretlint-disable-line -- fixture
		"path":     "https://litellm.example.com/sup3r-s3cret/v1",                  // secretlint-disable-line -- fixture
		"both":     "https://u:sup3r-s3cret@litellm.example.com/v1?k=sup3r-s3cret", // secretlint-disable-line -- fixture
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{AIProxyURL: raw}
			for _, formatted := range []string{cfg.String(), cfg.GoString(), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg)} {
				assert.NotContains(t, formatted, "sup3r-s3cret", "a credential inside AI_PROXY_URL must never reach a log line")
				assert.NotContains(t, formatted, "svcuser", "the username is part of the credential")
				// The host is masked too, so the exact rendered form is asserted:
				// only the scheme survives, and only after being checked against two
				// constants. Anything more reproduces operator input.
				assert.NotContains(t, formatted, "litellm.example.com", "the host is operator input and can itself be the secret")
				assert.Contains(t, formatted, `AIProxyURL:"https://xxxxx"`)
			}
		})
	}
}

// TestConfigString_AISettingsAgreeWithConstruction pins the diagnostic to what
// llm.NewClient will actually see. NewClient trims all three AI values and stores the
// normalized form, so a whitespace-only URL or key is NOT configured — it returns
// ErrNotConfigured. Printing the untrimmed originals made this string say the opposite:
// redactAIProxyURL's emptiness check ran before its TrimSpace, so "\n" rendered as
// "[redacted]", and redactSecret rendered a "\n" key as "xxxxx". Both read as CONFIGURED
// on the one line an operator consults to find out whether copy generation can run.
func TestConfigString_AISettingsAgreeWithConstruction(t *testing.T) {
	cfg := &Config{AIProxyURL: "\n", AIModel: "  gpt-4o-mini\n", AIAPIKey: " \t "}
	formatted := cfg.String()

	assert.Contains(t, formatted, `AIProxyURL:""`,
		"a whitespace-only proxy URL is unconfigured; [redacted] would report a proxy that NewClient rejects")
	assert.Contains(t, formatted, `AIAPIKey:""`,
		"a whitespace-only key is unconfigured; xxxxx would report a credential that is not there")
	assert.Contains(t, formatted, `AIModel:"gpt-4o-mini"`,
		"the model must be logged as it will be SENT, not as it was received")
}

// TestRedactAIProxyURL_Shapes covers the forms the value takes. Every http(s) case
// collapses to the same two-constant rendering, which is the point: the only component
// reproduced is a scheme this function has already checked equals "http" or "https", so
// no input can reach the log through it.
func TestRedactAIProxyURL_Shapes(t *testing.T) {
	cases := map[string]string{
		"":                                  "",
		"https://litellm.example.com":       "https://xxxxx",
		"https://litellm.example.com/v1/":   "https://xxxxx",
		"http://litellm:4000/v1":            "http://xxxxx",
		"https://u:p@litellm.example.com/v": "https://xxxxx", // secretlint-disable-line -- fixture
		"https://litellm.example.com?k=v":   "https://xxxxx",
		"https://litellm.example.com/v#f":   "https://xxxxx",
		// A token in a PATH SEGMENT. Parses cleanly, and the path is neither userinfo
		// nor a query, so an earlier scheme/host/path rebuild printed it verbatim.
		"https://litellm.example.com/sup3r-s3cret/v1": "https://xxxxx", // secretlint-disable-line -- fixture
		// A token AS THE HOST. This is the case the scheme+host form could not survive:
		// it is a well-formed absolute https URL whose entire content is the secret, so
		// there is no parse-level property that distinguishes it from a real endpoint.
		"https://sup3r-s3cret/":   "https://xxxxx", // secretlint-disable-line -- fixture
		"https://sup3r-s3cret:80": "https://xxxxx", // secretlint-disable-line -- fixture
		// Unparseable: no component this function can vouch for, so it masks.
		"https://litellm.example.com/%zz": "[redacted]",
		// Opaque/relative: the whole value sits in a field this does not render.
		"mailto:u:p@x": "[redacted]", // secretlint-disable-line -- fixture
		// A non-http(s) scheme with a HOST. It masks wholesale rather than rendering
		// `scheme://xxxxx`, because the scheme is the one component still reproduced
		// and an unrecognised one is exactly where the secret may be.
		"sup3r-s3cret://litellm.example.com": "[redacted]", // secretlint-disable-line -- fixture
	}
	for in, want := range cases {
		assert.Equal(t, want, redactAIProxyURL(in), "input %q", in)
	}
}

// TestRedactAIProxyURL_ReproducesNothingButTheScheme is the property behind the table:
// for any http(s) input, the output is drawn entirely from constants. Asserting it as a
// property rather than case by case is what stops the next round — a future component
// judged "structurally safe" fails here without anyone having to think of its fixture.
func TestRedactAIProxyURL_ReproducesNothingButTheScheme(t *testing.T) {
	const secret = "sup3r-s3cret" // secretlint-disable-line -- fixture
	for _, in := range []string{
		"https://" + secret,
		"https://" + secret + "/v1",
		"http://" + secret + ":4000/" + secret + "?k=" + secret + "#" + secret,
		"https://u:" + secret + "@" + secret + "/" + secret,
	} {
		got := redactAIProxyURL(in)
		assert.NotContains(t, got, secret, "input %q leaked through redaction", in)
		assert.Contains(t, []string{"https://xxxxx", "http://xxxxx", "[redacted]"}, got, "input %q", in)
	}
}

// TestRedactNATSURL_Shapes covers the forms a NATS URL actually takes, including the ones with
// nothing to redact (where masking would needlessly hide the host).
func TestRedactNATSURL_Shapes(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"nats://nats.lfx.svc:4222":      "nats://nats.lfx.svc:4222",
		"nats://u:p@nats.lfx.svc:4222":  "nats://***@nats.lfx.svc:4222", // secretlint-disable-line -- fixture
		"nats://token@nats.lfx.svc:422": "nats://***@nats.lfx.svc:422",
		"u:p@host:4222":                 "***@host:4222", // secretlint-disable-line -- fixture: no scheme
	}
	for in, want := range cases {
		assert.Equal(t, want, redactNATSURL(in), "input %q", in)
	}
}
