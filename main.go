//go:generate go install -v github.com/josephspurrier/goversioninfo/cmd/goversioninfo
//go:generate goversioninfo -icon=res/papp.ico
package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"text/template"

	"github.com/Jeffail/gabs"
	"github.com/pkg/errors"
	. "github.com/portapps/portapps"
	"github.com/portapps/portapps/pkg/dialog"
	"github.com/portapps/portapps/pkg/mutex"
	"github.com/portapps/portapps/pkg/utl"
)

type config struct {
	MultipleInstances bool   `yaml:"multiple_instances" mapstructure:"multiple_instances"`
	DisableTelemetry  bool   `yaml:"disable_telemetry" mapstructure:"disable_telemetry"`
	GnuPGAgentPath    string `yaml:"gnupg_agent_path" mapstructure:"gnupg_agent_path"`
	Locale            string `yaml:"locale" mapstructure:"locale"`
}

type policies struct {
	DisableAppUpdate        bool `json:"DisableAppUpdate"`
	DisableTelemetry        bool `json:"DisableTelemetry"`
	DontCheckDefaultBrowser bool `json:"DontCheckDefaultBrowser"`
}

var (
	app *App
	cfg *config
)

const (
	embeddedGnupgAgentPath = `app\gnupg\bin\gpg.exe`
	defaultLocale          = "en-US"
)

func init() {
	var err error

	// Default config
	cfg = &config{
		MultipleInstances: false,
		DisableTelemetry:  false,
		Locale:            defaultLocale,
	}

	// Init app
	if app, err = NewWithCfg("thunderbird-portable", "Thunderbird", cfg); err != nil {
		Log.Fatal().Err(err).Msg("Cannot initialize application. See log file for more info.")
	}
}

func main() {
	var err error
	utl.CreateFolder(app.DataPath)
	profileFolder := utl.CreateFolder(app.DataPath, "profile", "default")

	app.Process = utl.PathJoin(app.AppPath, "thunderbird.exe")
	app.Args = []string{
		"--profile",
		profileFolder,
	}

	// Locale
	locale, err := checkLocale()
	if err != nil {
		Log.Error().Err(err).Msg("Cannot set locale")
	}

	// GnuPG agent
	var gnupgAgentPath string
	Log.Info().Msg("Seeking GnuPG Agent path...")
	if cfg.GnuPGAgentPath != "" {
		gnupgAgentPath = cfg.GnuPGAgentPath
		Log.Info().Msgf("Getting GnuPG Agent from YAML cfg: %s", gnupgAgentPath)
	} else if gnupgAgentPath, err = exec.LookPath("gpg.exe"); err == nil {
		Log.Info().Msgf("Getting GnuPG Agent from PATH: %s", gnupgAgentPath)
	} else {
		gnupgAgentPath = utl.PathJoin(app.RootPath, embeddedGnupgAgentPath)
		Log.Info().Msgf("Getting embedded GnuPG Agent: %s", gnupgAgentPath)
	}
	if gnupgAgentPath != "" {
		gnupgAgentPath = strings.Replace(strings.Replace(gnupgAgentPath, `/`, `\`, -1), `\`, `\\`, -1)
	}

	// Multiple instances
	if cfg.MultipleInstances {
		Log.Info().Msg("Multiple instances enabled")
		app.Args = append(app.Args, "--no-remote")
	}

	// Policies
	distributionFolder := utl.CreateFolder(app.AppPath, "distribution")
	policies := struct {
		policies `json:"policies"`
	}{
		policies{
			DisableAppUpdate:        true,
			DisableTelemetry:        cfg.DisableTelemetry,
			DontCheckDefaultBrowser: true,
		},
	}
	rawPolicies, err := json.MarshalIndent(policies, "", "  ")
	if err != nil {
		Log.Fatal().Msg("Cannot marshal policies")
	}
	if err = ioutil.WriteFile(utl.PathJoin(distributionFolder, "policies.json"), rawPolicies, 0644); err != nil {
		Log.Fatal().Msg("Cannot write policies")
	}

	// Autoconfig
	prefFolder := utl.CreateFolder(app.AppPath, "defaults/pref")
	autoconfig := utl.PathJoin(prefFolder, "autoconfig.js")
	if err := utl.CreateFile(autoconfig, `//
pref("general.config.filename", "portapps.cfg");
pref("general.config.obscure_value", 0);`); err != nil {
		Log.Fatal().Err(err).Msg("Cannot write autoconfig.js")
	}

	// Mozilla cfg
	mozillaCfgPath := utl.PathJoin(app.AppPath, "portapps.cfg")
	mozillaCfgFile, err := os.Create(mozillaCfgPath)
	if err != nil {
		Log.Fatal().Err(err).Msg("Cannot create portapps.cfg")
	}
	mozillaCfgData := struct {
		Telemetry      string
		GnuPgAgentPath string
		Locale         string
	}{
		strconv.FormatBool(!cfg.DisableTelemetry),
		gnupgAgentPath,
		locale,
	}
	mozillaCfgTpl := template.Must(template.New("mozillaCfg").Parse(`// Disable updater
lockPref("app.update.enabled", false);
lockPref("app.update.auto", false);
lockPref("app.update.mode", 0);
lockPref("app.update.service.enabled", false);

// Set locale
pref("intl.locale.requested", "{{ .Locale }}");

// Extensions scopes
lockPref("extensions.enabledScopes", 4);
lockPref("extensions.autoDisableScopes", 3);

// Disable check default client
lockPref("mail.shell.checkDefaultClient", false);

// Disable WinSearch integration
lockPref("mail.winsearch.enable", false);
lockPref("mail.winsearch.firstRunDone", true);

// Disable Add-ons compatibility checking
clearPref("extensions.lastAppVersion");

// Don't show 'know your rights' on first run
pref("browser.rights.3.shown", true);
pref("mail.rights.version", 1);

// Disable start page
lockPref("mailnews.start_page.enabled", false);

// Disable calendar notification
lockPref("calendar.integration.notify", false);

// Don't show WhatsNew on first run after every update
pref("mailnews.start_page_override.mstone", "ignore");

// Disable health reporter
lockPref("datareporting.healthreport.service.enabled", {{ .Telemetry }});

// Disable all data upload (Telemetry and FHR)
lockPref("toolkit.telemetry.enabled", {{ .Telemetry }});
lockPref("datareporting.policy.dataSubmissionEnabled", {{ .Telemetry }});

// Disable crash reporter
lockPref("toolkit.crashreporter.enabled", false);

// Set enigmail GnuPG agent path
pref("extensions.enigmail.agentPath", "{{ .GnuPgAgentPath }}");
`))
	if err := mozillaCfgTpl.Execute(mozillaCfgFile, mozillaCfgData); err != nil {
		Log.Fatal().Err(err).Msg("Cannot write portapps.cfg")
	}

	// Fix extensions path
	if err := updateAddonStartup(profileFolder); err != nil {
		Log.Error().Err(err).Msg("Cannot fix extensions path")
	}

	// Set env vars
	crashreporterFolder := utl.CreateFolder(app.DataPath, "crashreporter")
	pluginsFolder := utl.CreateFolder(app.DataPath, "plugins")
	utl.OverrideEnv("MOZ_CRASHREPORTER", "0")
	utl.OverrideEnv("MOZ_CRASHREPORTER_DATA_DIRECTORY", crashreporterFolder)
	utl.OverrideEnv("MOZ_CRASHREPORTER_DISABLE", "1")
	utl.OverrideEnv("MOZ_CRASHREPORTER_NO_REPORT", "1")
	utl.OverrideEnv("MOZ_DATA_REPORTING", "0")
	utl.OverrideEnv("MOZ_MAINTENANCE_SERVICE", "0")
	utl.OverrideEnv("MOZ_PLUGIN_PATH", pluginsFolder)
	utl.OverrideEnv("MOZ_UPDATER", "0")

	// Create and check mutex
	mu, err := mutex.New(app.ID)
	defer mu.Release()
	if err != nil {
		if !cfg.MultipleInstances {
			Log.Error().Msg("You have to enable multiple instances in your configuration if you want to launch another instance")
			if _, err = dialog.MsgBox(
				fmt.Sprintf("%s portable", app.Name),
				"Other instance detected. You have to enable multiple instances in your configuration if you want to launch another instance.",
				dialog.MsgBoxBtnOk|dialog.MsgBoxIconError); err != nil {
				Log.Error().Err(err).Msg("Cannot create dialog box")
			}
			return
		} else {
			Log.Warn().Msg("Another instance is already running")
		}
	}

	app.Launch(os.Args[1:])
}

func checkLocale() (string, error) {
	extSourceFile := fmt.Sprintf("%s.xpi", cfg.Locale)
	extDestFile := fmt.Sprintf("langpack-%s@thunderbird.mozilla.org.xpi", cfg.Locale)
	extsFolder := utl.CreateFolder(app.AppPath, "distribution", "extensions")
	localeXpi := utl.PathJoin(app.AppPath, "langs", extSourceFile)

	// If default locale skip (already embedded)
	if cfg.Locale == defaultLocale {
		return cfg.Locale, nil
	}

	// Check .xpi file exists
	if !utl.Exists(localeXpi) {
		return defaultLocale, fmt.Errorf("XPI file does not exist in %s", localeXpi)
	}

	// Copy .xpi
	if err := utl.CopyFile(localeXpi, utl.PathJoin(extsFolder, extDestFile)); err != nil {
		return defaultLocale, err
	}

	return cfg.Locale, nil
}

func updateAddonStartup(profileFolder string) error {
	asLz4 := path.Join(profileFolder, "addonStartup.json.lz4")
	if !utl.Exists(asLz4) {
		return nil
	}

	decAsLz4, err := mozLz4Decompress(asLz4)
	if err != nil {
		return err
	}

	jsonAs, err := gabs.ParseJSON(decAsLz4)
	if err != nil {
		return err
	}

	if err := updateAddons("app-global", utl.PathJoin(profileFolder, "extensions"), jsonAs); err != nil {
		return err
	}
	if err := updateAddons("app-profile", utl.PathJoin(profileFolder, "extensions"), jsonAs); err != nil {
		return err
	}
	if err := updateAddons("app-system-defaults", utl.PathJoin(app.AppPath, "browser", "features"), jsonAs); err != nil {
		return err
	}
	Log.Debug().Msgf("Updated addonStartup.json: %s", jsonAs.String())

	encAsLz4, err := mozLz4Compress(jsonAs.Bytes())
	if err != nil {
		return err
	}

	return ioutil.WriteFile(asLz4, encAsLz4, 0644)
}

func updateAddons(field string, basePath string, container *gabs.Container) error {
	if _, ok := container.Search(field, "path").Data().(string); !ok {
		return nil
	}
	if _, err := container.Set(basePath, field, "path"); err != nil {
		return errors.Wrap(err, fmt.Sprintf("couldn't set %s.path", field))
	}

	addons, _ := container.S(field, "addons").ChildrenMap()
	for key, addon := range addons {
		_, err := addon.Set(fmt.Sprintf("jar:file:///%s/%s.xpi!/", utl.FormatUnixPath(basePath), url.PathEscape(key)), "rootURI")
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("couldn't set %s %s.rootURI", field, key))
		}
	}

	return nil
}
