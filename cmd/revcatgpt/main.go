package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/Yamashou/gqlgenc/clientv2"
	"github.com/je4/revcat/v2/tools/client"
	"github.com/je4/revcatgpt/v2/config"
	"github.com/je4/revcatgpt/v2/data/certs"
	"github.com/je4/revcatgpt/v2/pkg/revcatgtpservice"
	"github.com/je4/utils/v2/pkg/zLogger"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/rs/zerolog"
	"github.com/sashabaranov/go-openai"
	"golang.org/x/text/language"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var configfile = flag.String("config", "", "location of toml configuration file")

func auth(apikey string) func(ctx context.Context, req *http.Request, gqlInfo *clientv2.GQLRequestInfo, res interface{}, next clientv2.RequestInterceptorFunc) error {
	return func(ctx context.Context, req *http.Request, gqlInfo *clientv2.GQLRequestInfo, res interface{}, next clientv2.RequestInterceptorFunc) error {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apikey))
		return next(ctx, req, gqlInfo, res)
	}
}

func main() {

	flag.Parse()

	var cfgFS fs.FS
	var cfgFile string
	if *configfile != "" {
		cfgFS = os.DirFS(filepath.Dir(*configfile))
		cfgFile = filepath.Base(*configfile)
	} else {
		cfgFS = config.ConfigFS
		cfgFile = "revcatgpt.toml"
	}

	conf := &RevCatGPTConfig{
		LogFile:      "",
		LogLevel:     "DEBUG",
		LocalAddr:    "localhost:81",
		ExternalAddr: "http://localhost:81",
	}

	if err := LoadRevCatGPTConfig(cfgFS, cfgFile, conf); err != nil {
		log.Fatalf("cannot load toml from [%v] %s: %v", cfgFS, cfgFile, err)
	}

	// create logger instance
	var out io.Writer = os.Stdout
	if conf.LogFile != "" {
		fp, err := os.OpenFile(conf.LogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("cannot open logfile %s: %v", conf.LogFile, err)
		}
		defer fp.Close()
		out = fp
	}

	//	output := zerolog.ConsoleWriter{Out: out, TimeFormat: time.RFC3339}
	_logger := zerolog.New(out).With().Timestamp().Logger()
	switch strings.ToUpper(conf.LogLevel) {
	case "DEBUG":
		_logger = _logger.Level(zerolog.DebugLevel)
	case "INFO":
		_logger = _logger.Level(zerolog.InfoLevel)
	case "WARN":
		_logger = _logger.Level(zerolog.WarnLevel)
	case "ERROR":
		_logger = _logger.Level(zerolog.ErrorLevel)
	case "FATAL":
		_logger = _logger.Level(zerolog.FatalLevel)
	case "PANIC":
		_logger = _logger.Level(zerolog.PanicLevel)
	default:
		_logger = _logger.Level(zerolog.DebugLevel)
	}
	var logger zLogger.ZLogger = &_logger

	var localeFS fs.FS
	if conf.Locale.Folder == "" {
		localeFS = cfgFS
	} else {
		localeFS = os.DirFS(conf.Locale.Folder)
	}

	glang, err := language.Parse(conf.Locale.Default)
	if err != nil {
		logger.Fatal().Msgf("cannot parse language %s: %v", conf.Locale.Default, err)
	}
	bundle := i18n.NewBundle(glang)
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	for _, lang := range conf.Locale.Available {
		localeFile := fmt.Sprintf("active.%s.toml", lang)
		if _, err := fs.Stat(localeFS, localeFile); err != nil {
			logger.Fatal().Msgf("cannot find locale file [%v] %s", localeFS, localeFile)
		}

		if _, err := bundle.LoadMessageFileFS(localeFS, localeFile); err != nil {
			logger.Fatal().Msgf("cannot load locale file [%v] %s: %v", localeFS, localeFile, err)
		}

	}

	var cert *tls.Certificate
	if conf.TLSCert != "" {
		c, err := tls.LoadX509KeyPair(conf.TLSCert, conf.TLSKey)
		if err != nil {
			logger.Fatal().Msgf("cannot load tls certificate: %v", err)
		}
		cert = &c
	} else {
		if strings.HasPrefix(strings.ToLower(conf.ExternalAddr), "https://") {
			certBytes, err := fs.ReadFile(certs.CertFS, "localhost.cert.pem")
			if err != nil {
				logger.Fatal().Msgf("cannot read internal cert")
			}
			keyBytes, err := fs.ReadFile(certs.CertFS, "localhost.key.pem")
			if err != nil {
				logger.Fatal().Msgf("cannot read internal key")
			}
			c, err := tls.X509KeyPair(certBytes, keyBytes)
			if err != nil {
				logger.Fatal().Msgf("cannot create internal cert")
			}
			cert = &c
		}
	}
	if conf.Revcat.Insecure {
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	httpClient := &http.Client{}
	revcatClient := client.NewClient(httpClient, conf.Revcat.Endpoint, nil, func(ctx context.Context, req *http.Request, gqlInfo *clientv2.GQLRequestInfo, res interface{}, next clientv2.RequestInterceptorFunc) error {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", conf.Revcat.Apikey))
		return next(ctx, req, gqlInfo, res)
	})

	openaiClient := openai.NewClient(string(conf.OpenaiApiKey))

	ctrl, err := revcatgtpservice.NewController(
		conf.LocalAddr,
		conf.ExternalAddr,
		cert,
		revcatClient,
		bundle,
		openaiClient,
		logger)
	if err != nil {
		logger.Fatal().Msgf("cannot start initialize server: %v", err)
	}
	wg := &sync.WaitGroup{}

	ctrl.Start(wg)

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	fmt.Println("press ctrl+c to stop server")
	s := <-done
	fmt.Println("got signal:", s)

	ctrl.GracefulStop()

	wg.Wait()
}
