package main

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
	"go.uber.org/dig"
	"go.uber.org/fx"
	"net"
	"net/http"
	"os"
	"time"
)

type AppConfigProvider interface {
	Config() AppConfig
}

type AppLoader struct {
	cfg *AppConfig
	app *fx.App
}

func LoadApp(cfgPrefix string, appProvider fx.Option) (*AppLoader, error) {
	l := AppLoader{}

	if err := l.createApp(cfgPrefix, appProvider); err != nil {
		return nil, err
	}

	return &l, nil
}

func (l *AppLoader) createApp(cfgPrefix string, appProvider fx.Option) (err error) {
	l.cfg, err = l.loadCurrentConfigFromEnv(cfgPrefix)
	if err != nil {
		return errors.Wrap(err, "failed to load current config from env")
	}

	appOptions := fx.Options(
		fx.StartTimeout(l.cfg.StartTimeout),
		fx.StopTimeout(l.cfg.StopTimeout),
		fx.Provide(
			l.Config,
			func() AppConfigProvider { return l },
		),
		appProvider,
	)

	l.app = fx.New(appOptions)
	err = l.app.Err()
	if err == nil {
		return nil
	}

	err, ok := unwrapBadConfigError(err)
	if !ok {
		return errors.Wrap(err, "failed to create app with current config")
	}

	configError := err
	l.cfg, err = l.loadLastKnownGoodConfig()
	if err != nil {
		return errors.Wrap(err, "failed to fall back to last known good config")
	}
	l.cfg.UsesFallbackConfig = true
	l.cfg.ConfigError = configError.Error()

	l.app = fx.New(appOptions)
	err = l.app.Err()
	if err != nil {
		return errors.Wrap(err, "failed to create app with last known good config")
	}

	return nil
}

func (l *AppLoader) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		if err := l.app.Start(ctx); err != nil {
			cancel()
		}
	}()

	if !l.cfg.UsesFallbackConfig {
		if err := l.saveConfig(*l.cfg); err != nil {
			cancel()
			return errors.Wrap(err, "failed to save current config")
		}
	}

	<-l.app.Done()
	cancel()

	return nil
}

func (l *AppLoader) loadCurrentConfigFromEnv(appConfigPrefix string) (*AppConfig, error) {
	cfg, err := l.loadAppConfig(appConfigPrefix)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func (l *AppLoader) loadAppConfig(appConfigPrefix string) (*AppConfig, error) {
	cfg := &AppConfig{}
	if err := envconfig.Process(appConfigPrefix, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (l *AppLoader) loadLastKnownGoodConfig() (*AppConfig, error) {
	f, err := os.Open("last_known_good_config")
	if err != nil {
		return nil, err
	}
	cfg := &AppConfig{}
	if err := gob.NewDecoder(f).Decode(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (l *AppLoader) saveConfig(cfg AppConfig) error {
	f, err := os.Create("last_known_good_config")
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(cfg); err != nil {
		_ = f.Close()
		return err
	}

	_ = f.Close()
	return nil
}

func (l *AppLoader) Config() AppConfig {
	return *l.cfg
}

type BadConfigError struct {
	err error
}

func (e BadConfigError) Error() string {
	return fmt.Sprintf("bad config: %s", e.err.Error())
}

func unwrapBadConfigError(err error) (error, bool) {
	badConfigErr, ok := dig.RootCause(err).(BadConfigError)
	if !ok {
		return err, false
	}
	return badConfigErr, true
}

// все что ниже - это пример приложения, которое запускается через AppLoader

type echoServer struct {
	lis     net.Listener
	handler http.Handler
}

func newEchoServer(addr string, handler http.Handler) (*echoServer, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := echoServer{
		lis:     lis,
		handler: handler,
	}
	return &s, nil
}

func (s *echoServer) Start(_ context.Context) error {
	return http.Serve(s.lis, s.handler)
}

func (s *echoServer) Stop(_ context.Context) error {
	return s.lis.Close()
}

func ProvideApp() fx.Option {
	return fx.Options(
		fx.Provide(
			func(cfg AppConfig, configProvider AppConfigProvider) *echoHandler {
				return &echoHandler{
					configProvider: configProvider,
					respTimeout:    cfg.EchoHandler.ResponseTimeout,
				}
			},
			func(cfg AppConfig, handler *echoHandler) (*echoServer, error) {
				host := cfg.Server.Host
				if host == "" {
					return nil, BadConfigError{errors.New("server host can't be empty")}
				}
				port := cfg.Server.Port
				if port > 8999 || port < 8000 {
					return nil, BadConfigError{errors.New("server port should be between 8000 and 8999")}
				}
				addr := fmt.Sprintf("%s:%d", host, port)
				return newEchoServer(addr, handler)
			},
		),
		fx.Invoke(
			func(lifecycle fx.Lifecycle, server *echoServer) {
				lifecycle.Append(
					fx.Hook{
						OnStart: server.Start,
						OnStop:  server.Stop,
					})
			},
		),
	)
}

type AppConfig struct {
	UsesFallbackConfig bool          `json:"uses_fallback_config"`
	ConfigError        string        `json:"config_error,omitempty"`
	StartTimeout       time.Duration `envconfig:"loader_start_timeout"`
	StopTimeout        time.Duration `envconfig:"loader_stop_timeout"`

	EchoHandler EchoHandlerConfig `envconfig:"echo_handler"`
	Server      ServerConfig      `envconfig:"server"`
}

type EchoHandlerConfig struct {
	ResponseTimeout time.Duration `envconfig:"response_timeout"`
}

type ServerConfig struct {
	Host string `envconfig:"host"`
	Port int    `envconfig:"port"`
}

type echoHandler struct {
	respTimeout    time.Duration
	configProvider AppConfigProvider
}

func (e *echoHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	time.Sleep(e.respTimeout)
	b, err := json.Marshal(e.configProvider.Config())
	if err != nil {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	_, _ = w.Write(b)
}

func main() {
	appLoader, err := LoadApp("APP", ProvideApp())
	if err != nil {
		panic(err)
	}
	if err := appLoader.Start(context.Background()); err != nil {
		panic(err)
	}
}
