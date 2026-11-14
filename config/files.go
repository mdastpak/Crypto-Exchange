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
// otp.html for a sign-in code and withdrawal.html for the code that
// confirms a withdrawal (#161). notifications.email.html_templates chooses
// the language.
//
//go:embed email
var EmailTemplates embed.FS
