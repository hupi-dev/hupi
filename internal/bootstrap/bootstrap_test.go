package bootstrap

import "testing"

func TestResolveDatabaseURL(t *testing.T) {
	t.Run("prefers HUPI_APP_DATABASE_URL when set", func(t *testing.T) {
		t.Setenv("HUPI_APP_DATABASE_URL", "postgres://hupi_app@localhost/hupi")
		t.Setenv("HUPI_DATABASE_URL", "postgres://postgres@localhost/hupi")

		got, err := resolveDatabaseURL()
		if err != nil {
			t.Fatalf("resolveDatabaseURL: %v", err)
		}
		if got != "postgres://hupi_app@localhost/hupi" {
			t.Errorf("resolveDatabaseURL() = %q, want the app-role URL", got)
		}
	})

	t.Run("falls back to HUPI_DATABASE_URL when the app role isn't set", func(t *testing.T) {
		t.Setenv("HUPI_APP_DATABASE_URL", "")
		t.Setenv("HUPI_DATABASE_URL", "postgres://postgres@localhost/hupi")

		got, err := resolveDatabaseURL()
		if err != nil {
			t.Fatalf("resolveDatabaseURL: %v", err)
		}
		if got != "postgres://postgres@localhost/hupi" {
			t.Errorf("resolveDatabaseURL() = %q, want the fallback URL", got)
		}
	})

	t.Run("errors when neither is set", func(t *testing.T) {
		t.Setenv("HUPI_APP_DATABASE_URL", "")
		t.Setenv("HUPI_DATABASE_URL", "")

		if _, err := resolveDatabaseURL(); err == nil {
			t.Fatal("resolveDatabaseURL: expected an error when neither env var is set")
		}
	})
}

func TestRequireEnv(t *testing.T) {
	t.Run("returns the value when set", func(t *testing.T) {
		t.Setenv("HUPI_TEST_REQUIRE_ENV_VAR", "a-value")
		got, err := requireEnv("HUPI_TEST_REQUIRE_ENV_VAR")
		if err != nil {
			t.Fatalf("requireEnv: %v", err)
		}
		if got != "a-value" {
			t.Errorf("requireEnv() = %q, want %q", got, "a-value")
		}
	})

	t.Run("errors when unset", func(t *testing.T) {
		t.Setenv("HUPI_TEST_REQUIRE_ENV_VAR", "")
		if _, err := requireEnv("HUPI_TEST_REQUIRE_ENV_VAR"); err == nil {
			t.Fatal("requireEnv: expected an error for an unset variable")
		}
	})
}

func TestConfigPath(t *testing.T) {
	t.Run("defaults to providers.yaml", func(t *testing.T) {
		t.Setenv("HUPI_PROVIDERS_CONFIG", "")
		if got := configPath(); got != "providers.yaml" {
			t.Errorf("configPath() = %q, want %q", got, "providers.yaml")
		}
	})

	t.Run("honors HUPI_PROVIDERS_CONFIG when set", func(t *testing.T) {
		t.Setenv("HUPI_PROVIDERS_CONFIG", "/etc/hupi/providers.yaml")
		if got := configPath(); got != "/etc/hupi/providers.yaml" {
			t.Errorf("configPath() = %q, want %q", got, "/etc/hupi/providers.yaml")
		}
	})
}
