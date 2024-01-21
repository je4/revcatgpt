package main

import (
	"emperror.dev/errors"
	"github.com/BurntSushi/toml"
	"github.com/je4/utils/v2/pkg/config"
	"io/fs"
	"os"
)

type RevcatConfig struct {
	Endpoint string           `toml:"endpoint"`
	Insecure bool             `toml:"insecure"`
	Apikey   config.EnvString `toml:"apikey"`
}

type Directus struct {
	BaseUrl   string           `toml:"baseurl"`
	Token     config.EnvString `toml:"token"`
	CacheTime config.Duration  `toml:"cachetime"`
	CatalogID int              `toml:"catalogid"`
}

type LocaleConfig struct {
	Default   string   `toml:"default"`
	Folder    string   `toml:"folder"`
	Available []string `toml:"available"`
}

type RevCatGPTConfig struct {
	LocalAddr    string `toml:"localaddr"`
	ExternalAddr string `toml:"externaladdr"`
	TLSCert      string `toml:"tlscert"`
	TLSKey       string `toml:"tlskey"`

	Templates   string `toml:"templates"`
	StaticFiles string `toml:"staticfiles"`

	Locale LocaleConfig `toml:"locale"`

	LogFile  string `toml:"logfile"`
	LogLevel string `toml:"loglevel"`

	Revcat          RevcatConfig `toml:"revcat"`
	Directus        Directus     `toml:"directus"`
	MediaserverBase string       `toml:"mediaserverbase"`

	OpenaiApiKey config.EnvString `toml:"openaiapikey"`
}

func LoadRevCatGPTConfig(fSys fs.FS, fp string, conf *RevCatGPTConfig) error {
	if _, err := fs.Stat(fSys, fp); err != nil {
		path, err := os.Getwd()
		if err != nil {
			return errors.Wrap(err, "cannot get current working directory")
		}
		fSys = os.DirFS(path)
		fp = "revcatfront.toml"
	}
	data, err := fs.ReadFile(fSys, fp)
	if err != nil {
		return errors.Wrapf(err, "cannot read file [%v] %s", fSys, fp)
	}
	_, err = toml.Decode(string(data), conf)
	if err != nil {
		return errors.Wrapf(err, "error loading config file %v", fp)
	}
	return nil
}
