// Package config holds the project's configuration files: the example and
// Compose configurations of the server, and the data files that settings
// point to, such as the asset registry. Its only code embeds the example
// configuration, which the server falls back to when no configuration file
// exists: go:embed only reaches files below the package's directory. The
// configuration is loaded by internal/config.
package config

import _ "embed"

// Example is config.example.yaml.
//
//go:embed config.example.yaml
var Example []byte
