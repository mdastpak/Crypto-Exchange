// Package config holds the project's configuration files: the example and
// Compose configurations of the server, and the data files that settings
// point to, such as the asset registry. Its only code embeds the files that
// the server falls back to: the example configuration, when no configuration
// file exists, and the HTML templates of the emails. go:embed only reaches
// files below the package's directory. The configuration is loaded by
// internal/config.
package config

import (
	"embed"
)

// Example is config.example.yaml.
//
//go:embed config.example.yaml
var Example []byte

// EmailTemplates are the HTML templates of the emails, in email/<language>:
// otp.html for a sign-in code, withdrawal.html for the code that
// confirms a withdrawal (#161), anti_phishing_reminder.html for the
// reminder to change the anti-phishing code (#375), denied.html for the
// notice of a "this was not me" link (#374), and delisting_notice.html for
// the notices of a delisting (#216).
// notifications.email.html_templates chooses
// the language.
//
//go:embed email
var EmailTemplates embed.FS

// AssetRegistry is assets.yaml, the asset registry of this release (#57):
// assets.auto_sync and `admin assets import` read it when
// assets.registry_path names no other file, as in a container, which has
// no copy of the repository's files.
//
//go:embed assets.yaml
var AssetRegistry []byte
