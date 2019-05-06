//go:generate go install -v github.com/josephspurrier/goversioninfo/cmd/goversioninfo
//go:generate goversioninfo -icon=res/papp.ico
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/template"

	. "github.com/portapps/portapps"
	"github.com/portapps/portapps/pkg/dialog"
	"github.com/portapps/portapps/pkg/mutex"
	"github.com/portapps/portapps/pkg/utl"
)

type config struct {
	MultipleInstances bool   `yaml:"multiple_instances" mapstructure:"multiple_instances"`
	DisableTelemetry  bool   `yaml:"disable_telemetry" mapstructure:"disable_telemetry"`
	GnuPGAgentPath    string `yaml:"gnupg_agent_path" mapstructure:"gnupg_agent_path"`
}

var (
	app *App
	cfg *config
)

const (
	embeddedGnupgAgentPath = `app\gnupg\bin\gpg.exe`
)

func init() {
	var err error

	// Default config
	cfg = &config{
		MultipleInstances: false,
		DisableTelemetry:  false,
	}

	// Init app
	if app, err = NewWithCfg("thunderbird-portable", "Thunderbird", cfg); err != nil {
		Log.Fatal().Err(err).Msg("Cannot initialize application. See log file for more info.")
	}
}

func main() {
	var err error
	utl.CreateFolder(app.DataPath)

	app.Process = utl.PathJoin(app.AppPath, "thunderbird.exe")
	app.Args = []string{
		"--profile",
		utl.CreateFolder(app.DataPath, "profile", "default"),
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
	}{
		strconv.FormatBool(!cfg.DisableTelemetry),
		gnupgAgentPath,
	}
	mozillaCfgTpl := template.Must(template.New("mozillaCfg").Parse(`// Disable updater
lockPref("app.update.enabled", false);
lockPref("app.update.auto", false);
lockPref("app.update.mode", 0);
lockPref("app.update.service.enabled", false);

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
