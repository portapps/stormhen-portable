//go:generate go install -v github.com/josephspurrier/goversioninfo/cmd/goversioninfo
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	"github.com/portapps/portapps/v3"
	"github.com/portapps/portapps/v3/pkg/files"
	"github.com/portapps/portapps/v3/pkg/log"
	"github.com/portapps/portapps/v3/pkg/mutex"
	"github.com/portapps/portapps/v3/pkg/win"
)

type config struct {
	Cleanup              bool   `yaml:"cleanup" mapstructure:"cleanup"`
	MultipleInstances    bool   `yaml:"multiple_instances" mapstructure:"multiple_instances"`
	DisableTelemetry     bool   `yaml:"disable_telemetry" mapstructure:"disable_telemetry"`
	DisableCrashReporter bool   `yaml:"disable_crash_reporter" mapstructure:"disable_crash_reporter"`
	GnuPGHome            string `yaml:"gnupg_home" mapstructure:"gnupg_home"`
	GnuPGPath            string `yaml:"gnupg_path" mapstructure:"gnupg_path"`
	Locale               string `yaml:"locale" mapstructure:"locale"`
}

var (
	app *portapps.App
	cfg *config
)

const defaultLocale = "en-US"

func init() {
	var err error

	// Default config
	cfg = &config{
		Cleanup:              false,
		MultipleInstances:    false,
		DisableTelemetry:     false,
		DisableCrashReporter: true,
		Locale:               defaultLocale,
	}

	// Init app
	if app, err = portapps.NewWithCfg("stormhen-portable", "Stormhen", cfg); err != nil {
		log.Fatal().Err(err).Msg("Cannot initialize application. See log file for more info.")
	}
}

func main() {
	var err error
	if err := os.MkdirAll(app.DataPath, os.ModePerm); err != nil {
		log.Fatal().Err(err).Msg("Cannot create data directory.")
	}
	profileFolder := filepath.Join(app.DataPath, "profile", "default")
	if err := os.MkdirAll(profileFolder, os.ModePerm); err != nil {
		log.Fatal().Err(err).Msg("Cannot create profile directory.")
	}

	app.Process = filepath.Join(app.AppPath, "thunderbird.exe")
	app.Args = []string{
		"-profile",
		profileFolder,
	}

	// Set env vars
	crashreporterFolder := filepath.Join(app.DataPath, "crashreporter")
	if err := os.MkdirAll(crashreporterFolder, os.ModePerm); err != nil {
		log.Fatal().Err(err).Msg("Cannot create crash reporter directory.")
	}
	pluginsFolder := filepath.Join(app.DataPath, "plugins")
	if err := os.MkdirAll(pluginsFolder, os.ModePerm); err != nil {
		log.Fatal().Err(err).Msg("Cannot create plugins directory.")
	}
	os.Setenv("MOZ_CRASHREPORTER_DATA_DIRECTORY", crashreporterFolder)
	os.Setenv("MOZ_MAINTENANCE_SERVICE", "0")
	os.Setenv("MOZ_PLUGIN_PATH", pluginsFolder)
	os.Setenv("MOZ_UPDATER", "0")
	if cfg.DisableCrashReporter {
		os.Setenv("MOZ_CRASHREPORTER", "0")
		os.Setenv("MOZ_CRASHREPORTER_DISABLE", "1")
		os.Setenv("MOZ_CRASHREPORTER_NO_REPORT", "1")
	}
	if cfg.DisableTelemetry {
		os.Setenv("MOZ_DATA_REPORTING", "0")
	}

	// Create and check mutex
	mu, err := mutex.Create(app.ID)
	if err != nil {
		if !cfg.MultipleInstances {
			log.Error().Msg("You have to enable multiple instances in your configuration if you want to launch another instance")
			if _, err = win.MsgBox(
				fmt.Sprintf("%s portable", app.Name),
				"Other instance detected. You have to enable multiple instances in your configuration if you want to launch another instance.",
				win.MsgBoxBtnOk|win.MsgBoxIconError); err != nil {
				log.Error().Err(err).Msg("Cannot create dialog box")
			}
			return
		} else {
			log.Warn().Msg("Another instance is already running")
		}
	} else {
		defer mutex.Release(mu)
	}

	// Cleanup on exit
	if cfg.Cleanup {
		defer func() {
			var paths []string
			if appData := os.Getenv("APPDATA"); appData != "" {
				paths = append(paths, filepath.Join(appData, "Thunderbird"))
			}
			if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
				paths = append(paths, filepath.Join(localAppData, "Thunderbird"))
			}
			if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
				paths = append(paths, filepath.Join(userProfile, "AppData", "LocalLow", "Thunderbird"))
			}
			files.Cleanup(paths...)
		}()
	}

	// Locale
	locale, err := checkLocale()
	if err != nil {
		log.Error().Err(err).Msg("Cannot set locale")
	}

	// GnuPG
	gnupgHome := cfg.GnuPGHome
	if gnupgHome == "" {
		gnupgHome = os.Getenv("GNUPGHOME")
	}
	if gnupgHome != "" {
		os.Setenv("GNUPGHOME", gnupgHome)
	}
	gnupgPath := cfg.GnuPGPath

	// Multiple instances
	if cfg.MultipleInstances {
		log.Info().Msg("Multiple instances enabled")
		app.Args = append(app.Args, "-no-remote")
	}

	// Policies
	if err := createPolicies(locale); err != nil {
		log.Fatal().Err(err).Msg("Cannot create policies")
	}

	// Autoconfig
	prefFolder := filepath.Join(app.AppPath, "defaults", "pref")
	if err := os.MkdirAll(prefFolder, os.ModePerm); err != nil {
		log.Fatal().Err(err).Msg("Cannot create preferences directory.")
	}
	autoconfig := filepath.Join(prefFolder, "autoconfig.js")
	if err := os.WriteFile(autoconfig, []byte(`//
pref("general.config.filename", "portapps.cfg");
pref("general.config.obscure_value", 0);`), 0644); err != nil {
		log.Fatal().Err(err).Msg("Cannot write autoconfig.js")
	}

	// Mozilla cfg
	mozillaCfgPath := filepath.Join(app.AppPath, "portapps.cfg")
	mozillaCfgFile, err := os.Create(mozillaCfgPath)
	if err != nil {
		log.Fatal().Err(err).Msg("Cannot create portapps.cfg")
	}
	mozillaCfgData := struct {
		DisableCrashReporter bool
		HasGnuPGPath         bool
		GnuPGPath            string
		Locale               string
	}{
		cfg.DisableCrashReporter,
		gnupgPath != "",
		strconv.Quote(gnupgPath),
		strconv.Quote(locale),
	}
	mozillaCfgTpl := template.Must(template.New("mozillaCfg").Parse(`// Portable defaults only.

// Locale fallback. Prefer policies.json RequestedLocales for modern Thunderbird.
pref("intl.locale.requested", {{ .Locale }});

// Keep first-run noise down.
pref("browser.rights.3.shown", true);
pref("mail.rights.version", 1);

{{ if .HasGnuPGPath -}}
// Use external OpenPGP GnuPG.
pref("mail.openpgp.allow_external_gnupg", true);
pref("mail.openpgp.alternative_gpg_path", {{ .GnuPGPath }});
pref("mail.openpgp.fetch_pubkeys_from_gnupg", true);

{{ end -}}
{{ if .DisableCrashReporter -}}
// Disable crash reporter
lockPref("toolkit.crashreporter.enabled", false);
{{ end -}}
`))
	if err := mozillaCfgTpl.Execute(mozillaCfgFile, mozillaCfgData); err != nil {
		mozillaCfgFile.Close()
		log.Fatal().Err(err).Msg("Cannot write portapps.cfg")
	}
	if err := mozillaCfgFile.Close(); err != nil {
		log.Fatal().Err(err).Msg("Cannot close portapps.cfg")
	}

	// Fix extensions path
	if err := updateAddonStartup(profileFolder); err != nil {
		log.Error().Err(err).Msg("Cannot fix extensions path")
	}

	defer app.Close()
	app.Launch(os.Args[1:])
}

func checkLocale() (string, error) {
	extSourceFile := fmt.Sprintf("%s.xpi", cfg.Locale)
	extDestFile := fmt.Sprintf("langpack-%s@thunderbird.mozilla.org.xpi", cfg.Locale)
	extsFolder := filepath.Join(app.AppPath, "distribution", "extensions")
	if err := os.MkdirAll(extsFolder, os.ModePerm); err != nil {
		return defaultLocale, err
	}
	localeXpi := filepath.Join(app.AppPath, "langs", extSourceFile)

	// If default locale skip (already embedded)
	if cfg.Locale == defaultLocale {
		return cfg.Locale, nil
	}

	// Check .xpi file exists
	if _, err := os.Stat(localeXpi); os.IsNotExist(err) {
		return defaultLocale, fmt.Errorf("XPI file does not exist in %s", localeXpi)
	}

	// Copy .xpi
	if err := files.CopyFile(localeXpi, filepath.Join(extsFolder, extDestFile)); err != nil {
		return defaultLocale, err
	}

	return cfg.Locale, nil
}

func createPolicies(locale string) error {
	distributionFolder := filepath.Join(app.AppPath, "distribution")
	if err := os.MkdirAll(distributionFolder, os.ModePerm); err != nil {
		return errors.Wrap(err, "Cannot create distribution folder")
	}
	appFile := filepath.Join(distributionFolder, "policies.json")
	dataFile := filepath.Join(app.DataPath, "policies.json")
	jsonPolicies := map[string]interface{}{
		"policies": map[string]interface{}{},
	}
	defaultPolicies, err := json.Marshal(jsonPolicies)
	if err != nil {
		return errors.Wrap(err, "Cannot marshal default policies")
	}
	log.Debug().Msgf("Default policies: %s", string(defaultPolicies))

	if _, err := os.Stat(dataFile); err == nil {
		rawCustomPolicies, err := os.ReadFile(dataFile)
		if err != nil {
			return errors.Wrap(err, "Cannot read custom policies")
		}
		if err := json.Unmarshal(rawCustomPolicies, &jsonPolicies); err != nil {
			return errors.Wrap(err, "Cannot consume custom policies")
		}
		customPolicies, err := json.Marshal(jsonPolicies)
		if err != nil {
			return errors.Wrap(err, "Cannot marshal custom policies")
		}
		log.Debug().Msgf("Custom policies: %s", string(customPolicies))
	}

	managedPrefs := map[string]struct {
		Value  interface{}
		Status string
	}{
		"calendar.integration.notify": {
			Value:  false,
			Status: "locked",
		},
		"mail.shell.checkDefaultClient": {
			Value:  false,
			Status: "locked",
		},
		"mail.winsearch.enable": {
			Value:  false,
			Status: "locked",
		},
		"mail.winsearch.firstRunDone": {
			Value:  true,
			Status: "locked",
		},
		"mailnews.start_page.enabled": {
			Value:  false,
			Status: "locked",
		},
		"mailnews.start_page_override.mstone": {
			Value:  "ignore",
			Status: "default",
		},
	}

	ensureObject := func(parent map[string]interface{}, key string) (map[string]interface{}, error) {
		if value, ok := parent[key]; ok {
			object, ok := value.(map[string]interface{})
			if !ok {
				return nil, errors.Errorf("%s must be an object", key)
			}
			return object, nil
		}
		object := map[string]interface{}{}
		parent[key] = object
		return object, nil
	}

	policies, err := ensureObject(jsonPolicies, "policies")
	if err != nil {
		return errors.Wrap(err, "Cannot consume policies")
	}
	preferences, err := ensureObject(policies, "Preferences")
	if err != nil {
		return errors.Wrap(err, "Cannot consume preferences policies")
	}

	policies["DisableAppUpdate"] = true

	if cfg.DisableTelemetry {
		policies["DisableTelemetry"] = true
	}
	if locale != "" {
		policies["RequestedLocales"] = locale
	}
	for name, pref := range managedPrefs {
		preference, err := ensureObject(preferences, name)
		if err != nil {
			return err
		}
		preference["Value"] = pref.Value
		preference["Status"] = pref.Status
	}

	appliedPolicies, err := json.MarshalIndent(jsonPolicies, "", "  ")
	if err != nil {
		return errors.Wrap(err, "Cannot marshal policies")
	}
	log.Debug().Msgf("Applied policies: %s", string(appliedPolicies))
	if err := os.WriteFile(appFile, appliedPolicies, 0644); err != nil {
		return errors.Wrap(err, "Cannot write policies")
	}

	return nil
}

func updateAddonStartup(profileFolder string) error {
	lz4File := filepath.Join(profileFolder, "addonStartup.json.lz4")
	if _, err := os.Stat(lz4File); os.IsNotExist(err) || app.Prev.RootPath == "" {
		return nil
	}

	lz4Raw, err := mozLz4Decompress(lz4File)
	if err != nil {
		return err
	}

	prevPathLin := escapedUnixPath(app.Prev.RootPath)
	currPathLin := escapedUnixPath(app.RootPath)
	lz4Str := strings.Replace(string(lz4Raw), prevPathLin, currPathLin, -1)

	prevPathWin := escapedWindowsPath(app.Prev.RootPath)
	currPathWin := escapedWindowsPath(app.RootPath)
	lz4Str = strings.Replace(lz4Str, prevPathWin, currPathWin, -1)

	lz4Enc, err := mozLz4Compress([]byte(lz4Str))
	if err != nil {
		return err
	}

	return os.WriteFile(lz4File, lz4Enc, 0644)
}

func escapedUnixPath(path string) string {
	return strings.ReplaceAll(filepath.ToSlash(path), ` `, `%20`)
}

func escapedWindowsPath(path string) string {
	return strings.ReplaceAll(strings.ReplaceAll(filepath.FromSlash(path), `\`, `\\`), ` `, `%20`)
}
