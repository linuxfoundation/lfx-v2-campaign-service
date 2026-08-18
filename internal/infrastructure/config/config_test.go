// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package config

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
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
		// nobody is checking. AIProxyURL and AIModel are populated too, but for opposite
		// reasons: the URL IS masked (the host is operator input and can itself be the
		// secret) down to a scheme-only `https://xxxxx`, which still answers "is copy
		// generation wired, and is the hop TLS?"; the model id is NOT masked, because it
		// is not a credential and an operator diagnosing bad copy needs it. The
		// assertions below pin both halves.
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

// TestConfigString_RedactsJWKSCredentials: the JWKS URL was printed verbatim by a formatter
// whose doc comment promises a log-safe representation. An issuer behind a gateway that
// authenticates with basic auth is an ordinary deployment, and nothing in this service forbids
// configuring one — so "a JWKS URL is public" is a convention, not a guarantee, and the
// formatter is exactly where a convention stops holding. The host is KEPT, for the same reason
// as the broker host: it is what makes a failing JWKS fetch diagnosable.
func TestConfigString_RedactsJWKSCredentials(t *testing.T) {
	cfg := &Config{JWKSUrl: "https://svcuser:sup3r-s3cret@auth.lfx.dev/.well-known/jwks.json"} // secretlint-disable-line -- fixture asserting the password is redacted

	for _, formatted := range []string{cfg.String(), cfg.GoString(), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg)} {
		assert.NotContains(t, formatted, "sup3r-s3cret", "the JWKS credential must never reach a log line")
		assert.NotContains(t, formatted, "svcuser", "the username is part of the credential")
		assert.Contains(t, formatted, "auth.lfx.dev/.well-known/jwks.json")
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

// TestRedactURLUserinfo_Shapes covers the forms these URLs actually take, including the ones
// with nothing to redact (where masking would needlessly hide the host).
func TestRedactURLUserinfo_Shapes(t *testing.T) {
	cases := map[string]string{
		"":                              "",
		"nats://nats.lfx.svc:4222":      "nats://nats.lfx.svc:4222",
		"nats://u:p@nats.lfx.svc:4222":  "nats://***@nats.lfx.svc:4222", // secretlint-disable-line -- fixture
		"nats://token@nats.lfx.svc:422": "nats://***@nats.lfx.svc:422",
		"u:p@host:4222":                 "***@host:4222", // secretlint-disable-line -- fixture: no scheme
		"https://auth.lfx.dev/.well-known/jwks.json":     "https://auth.lfx.dev/.well-known/jwks.json",
		"https://u:p@auth.lfx.dev/.well-known/jwks.json": "https://***@auth.lfx.dev/.well-known/jwks.json", // secretlint-disable-line -- fixture
	}
	for in, want := range cases {
		assert.Equal(t, want, redactURLUserinfo(in), "input %q", in)
	}
}

// TestRunningInCluster covers the discriminator auth.New uses to refuse the local
// mock-principal bypass in a deployment.
//
// The env variable alone is not trustworthy: the chart renders every app.environment key
// and appends app.extraEnv verbatim, and an explicit container env entry overrides the
// kubelet's service variables — so the one override that enables the bypass could also
// have cleared KUBERNETES_SERVICE_HOST. The chart now refuses to render that, and this
// pins the runtime half: the service-account directory is a second signal, so a manifest
// applied outside the chart cannot hide the cluster by clearing the variable either.
func TestRunningInCluster(t *testing.T) {
	saDir := t.TempDir()
	// A path that does not exist, for the "not in a cluster" cases.
	absent := filepath.Join(saDir, "no-such-dir")

	// A FILE at the service-account path is not a mounted token directory. Only a
	// directory counts, so a stray file cannot fabricate a cluster.
	saFile := filepath.Join(saDir, "not-a-dir")
	if err := os.WriteFile(saFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	env := func(v string) func(string) string {
		return func(k string) string {
			if k == constants.EnvKubernetesServiceHost {
				return v
			}
			return ""
		}
	}

	tests := []struct {
		name  string
		host  string
		saDir string
		want  bool
	}{
		{"neither signal: a developer's laptop", "", absent, false},
		{"kubelet variable only", "10.96.0.1", absent, true},
		// THE case the guard exists for: the deploy cleared the variable, and the
		// service-account mount still gives it away.
		{"service-account mount only, variable cleared", "", saDir, true},
		{"both signals", "10.96.0.1", saDir, true},
		{"a file at the service-account path is not a mount", "", saFile, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := runningInCluster(env(tc.host), tc.saDir); got != tc.want {
				t.Errorf("runningInCluster(host=%q, saDir=%q) = %v, want %v",
					tc.host, tc.saDir, got, tc.want)
			}
		})
	}
}

// TestResolveDatabaseURL: the subcommand must compose the DSN the server does from the PG* the
// chart injects, not read DATABASE_URL, unset in-cluster. PGPORT/PGENGINE are pinned rather than
// omitted: ambient values would change the expected DSN or fail validation before the assertion.
func TestResolveDatabaseURL(t *testing.T) {
	t.Setenv("PGHOST", "db.internal")
	t.Setenv("PGPORT", "5432")
	t.Setenv("PGENGINE", "postgres")
	t.Setenv("PGUSER", "svc")
	t.Setenv("PGPASSWORD", "p@ss word")
	t.Setenv("PGDATABASE", "campaigns")
	dsn, err := ResolveDatabaseURL()
	assert.NoError(t, err)
	assert.Equal(t, "postgres://svc:p%40ss%20word@db.internal:5432/campaigns", dsn) // secretlint-disable-line -- intentional fake DSN
}

// TestResolveDatabaseURL_PGVarsWinOverDatabaseURL makes the precedence a decision rather
// than an accident. ResolveDatabaseURL seeds DatabaseURL from DATABASE_URL and then
// loadDatabaseFromEnv OVERWRITES it whenever the PG* set is complete, so PG* wins — which
// is right for the deployment this service actually has (the chart injects PG* and leaves
// DATABASE_URL unset), but nothing said so, and both orderings look equally plausible
// reading the two functions.
//
// It matters because the server and the bootstrap-system-account subcommand resolve the
// DSN through this same function. Were they ever to disagree, the subcommand would write
// the LF system credential into one database while the server read from another, and the
// symptom would be a connection that is simply "not there" — not an error either side
// could report.
func TestResolveDatabaseURL_PGVarsWinOverDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://other:other@elsewhere:5432/other") // secretlint-disable-line -- intentional fake DSN
	t.Setenv("PGHOST", "db.internal")
	t.Setenv("PGPORT", "5432")
	t.Setenv("PGENGINE", "postgres")
	t.Setenv("PGUSER", "svc")
	t.Setenv("PGPASSWORD", "pw")
	t.Setenv("PGDATABASE", "campaigns")

	dsn, err := ResolveDatabaseURL()
	assert.NoError(t, err)
	assert.Equal(t, "postgres://svc:pw@db.internal:5432/campaigns", dsn, // secretlint-disable-line -- intentional fake DSN
		"a complete PG* set must win over DATABASE_URL: the chart injects PG*, so the "+
			"in-cluster value has to be the one that takes effect")
}

// TestResolveDatabaseURL_IncompletePGVarsAreRefusedNotIgnored pins the other half, and it
// is the half that surprises: a PARTIAL PG* set is an ERROR, not a quiet fall-through to
// DATABASE_URL. `loadDatabaseFromEnv` composes a DSN only when host, user, password and
// database are all present, but `ValidateDatabaseSettings` then refuses a set that is
// non-empty and incomplete, so the explicit DATABASE_URL is never reached.
//
// That is the right answer and worth pinning precisely because the alternative reads as
// friendlier. Silently preferring DATABASE_URL when PG* is half-set would mean a chart
// revision that drops PGPASSWORD quietly redirects the service to whatever DATABASE_URL
// happens to hold — the class of failure where everything starts and the data goes
// somewhere else. Refusing to start says so at the only moment anyone is watching.
func TestResolveDatabaseURL_IncompletePGVarsAreRefusedNotIgnored(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://svc:pw@explicit:5432/campaigns") // secretlint-disable-line -- intentional fake DSN
	t.Setenv("PGHOST", "db.internal")
	t.Setenv("PGUSER", "svc")
	// PGPASSWORD and PGDATABASE are set EMPTY rather than left unset: every reader here
	// tests for emptiness (loadDatabaseFromEnv: `password != ""`, and TrimSpace on the
	// rest), so empty and absent are the same input — while "left unset" would inherit
	// whatever the developer's shell or a CI job with a Postgres service exports. A test
	// for a partial set must not depend on the ambient environment being empty; there it
	// would silently become a COMPLETE set and stop exercising this branch at all.
	// PGPORT and PGENGINE are cleared for the same reason: an ambient value would change
	// which validation arm is reached.
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGDATABASE", "")
	t.Setenv("PGPORT", "")
	t.Setenv("PGENGINE", "")

	dsn, err := ResolveDatabaseURL()
	require.Error(t, err, "a partial PG* set must be refused rather than silently redirecting "+
		"the service to whatever DATABASE_URL holds")
	assert.Empty(t, dsn)
	assert.Contains(t, err.Error(), "PGPASSWORD")
	assert.Contains(t, err.Error(), "PGDATABASE")
	assert.NotContains(t, err.Error(), "explicit", "the error must not echo the DSN")
}

// TestParseRetention pins that EVERY unusable input falls back to 0 ("use the long default"),
// never to a short window.
//
// CAMPAIGN_JOB_RETENTION governs deletion of campaign_jobs rows, which are the audit trail of
// real ad spend. The asymmetry matters: falling back keeps MORE history than asked for, which
// is safe, while any reading of a malformed value as a short window silently destroys records.
// "30 days" and "7d" are included because they are the plausible typos — neither is a valid Go
// duration, and both would otherwise be tempting to coerce.
func TestParseRetention(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want time.Duration
	}{
		{"unset", "", 0},
		{"whitespace", "   ", 0},
		{"unparseable words", "30 days", 0},
		{"unparseable shorthand", "7d", 0},
		{"garbage", "forever", 0},
		{"zero", "0h", 0},
		{"negative", "-24h", 0},
		{"valid hours", "4320h", 4320 * time.Hour},
		{"valid compound", "1h30m", 90 * time.Minute},
		{"surrounding whitespace is trimmed", "  720h  ", 720 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseRetention(tc.in),
				"a value this service cannot use must fall back to the long default, never to a short window")
		})
	}
}

// TestLoadCampaignJobRetentionFromEnv pins the wiring from the environment variable to the
// Config field, so a rename or a dropped assignment cannot leave the setting silently inert.
//
// It calls LoadConfig, which is the whole point: an earlier version of this test asserted
// parseRetention(os.Getenv(...)) directly, which re-implements the line under test instead of
// running it. Deleting the CampaignJobRetention assignment from LoadConfig left that version
// green — the operator's window would reach nothing, and the feature would be inert with a
// passing wiring test. Only reading the field off the value LoadConfig returns can catch that.
func TestLoadCampaignJobRetentionFromEnv(t *testing.T) {
	// LoadConfig registers its flags on the global flag.CommandLine and calls flag.Parse, so a
	// second call would panic with "flag redefined" and the first would try to parse the test
	// binary's own arguments. Both are replaced for the duration of this test and restored
	// after, which is what makes calling the real function from a test viable at all.
	loadConfig := func() *Config {
		t.Helper()
		orig := flag.CommandLine
		origArgs := os.Args
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
		os.Args = []string{origArgs[0]}
		defer func() { flag.CommandLine, os.Args = orig, origArgs }()
		return LoadConfig()
	}

	t.Setenv(constants.EnvCampaignJobRetention, "720h")
	assert.Equal(t, 720*time.Hour, loadConfig().CampaignJobRetention,
		"CAMPAIGN_JOB_RETENTION must reach Config.CampaignJobRetention through LoadConfig itself")

	// The name is part of the contract: the chart and the docs both spell it out, so a rename
	// here is a silent break for every deployment that sets it.
	assert.Equal(t, "CAMPAIGN_JOB_RETENTION", constants.EnvCampaignJobRetention)

	// The fallback arm, also through LoadConfig: an unusable value must leave the field at 0
	// ("use the long repository default"), never at a short window.
	t.Setenv(constants.EnvCampaignJobRetention, "30 days")
	assert.Equal(t, time.Duration(0), loadConfig().CampaignJobRetention,
		"an unparseable window must fall back to the repository default, never to a short window")
}
