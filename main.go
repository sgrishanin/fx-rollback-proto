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

type ConfigProvider interface {
	Config() Config
}

type AppLoader struct {
	cfg *Config
	app *fx.App
}

type Config struct {
	// Здесь содержатся конфиги для AppLoader
	LoaderConfig
	// Здесь лежит указатель на конфиг самого приложения
	App interface{} `json:"app_config"`
}

type LoaderConfig struct {
	UsesFallbackConfig   bool          `json:"loader_uses_fallback_config"`
	IgnoreFallbackConfig bool          `envconfig:"loader_ignore_fallback_config" json:"loader_ignore_fallback_config"`
	ConfigError          string        `json:"loader_config_error,omitempty"`
	StartTimeout         time.Duration `envconfig:"loader_start_timeout" json:"loader_start_timeout"`
	StopTimeout          time.Duration `envconfig:"loader_stop_timeout" json:"loader_stop_timeout"`
}

func LoadApp(cfgPrefix string, appProvider fx.Option, appConfigPtr interface{}) (*AppLoader, error) {
	l := AppLoader{}

	if err := l.createApp(cfgPrefix, appProvider, appConfigPtr); err != nil {
		return nil, err
	}

	return &l, nil
}

func (l *AppLoader) createApp(cfgPrefix string, appProvider fx.Option, appConfigPtr interface{}) (err error) {
	l.cfg = &Config{
		App: appConfigPtr,
	}
	if err := l.initLoaderConfigFromEnv(); err != nil {
		return errors.Wrap(err, "failed to init loader config")
	}

	err = l.loadCurrentConfigFromEnv(cfgPrefix)
	if err != nil {
		return errors.Wrap(err, "failed to load current config from env")
	}

	appOptions := fx.Options(
		fx.StartTimeout(l.cfg.StartTimeout),
		fx.StopTimeout(l.cfg.StopTimeout),
		fx.Provide(
			l.Config,
			func() ConfigProvider { return l },
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

	if l.cfg.LoaderConfig.IgnoreFallbackConfig {
		return errors.New("failed to create app with current config and fallback config is ignored")
	}

	configError := err
	err = l.loadLastKnownGoodConfig()
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
		if err := l.saveConfig(); err != nil {
			cancel()
			return errors.Wrap(err, "failed to save current config")
		}
	}

	<-l.app.Done()
	cancel()

	return nil
}

const (
	loaderConfigPrefix = "LOADER"

	defaultLoaderStartTimeout = time.Second * 60
	defaultLoaderStopTimeout  = time.Second * 60
)

// загружает конфиги самого AppLoader и проставляет дефолтные значения
func (l *AppLoader) initLoaderConfigFromEnv() error {
	if err := envconfig.Process(loaderConfigPrefix, &l.cfg.LoaderConfig); err != nil {
		return err
	}

	if l.cfg.LoaderConfig.StartTimeout == 0 {
		l.cfg.LoaderConfig.StartTimeout = defaultLoaderStartTimeout
	}
	if l.cfg.LoaderConfig.StopTimeout == 0 {
		l.cfg.LoaderConfig.StopTimeout = defaultLoaderStopTimeout
	}

	return nil
}

// загружает актуальные конфиги приложения
// todo абстрагировать этот код чтобы можно было грузить не только из env но и из других источников (ejm и тд)
func (l *AppLoader) loadCurrentConfigFromEnv(appConfigPrefix string) error {
	if err := envconfig.Process(appConfigPrefix, l.cfg.App); err != nil {
		return err
	}
	return nil
}

// загружает последний известный рабочий конфиг
// todo абстрагировать для сохранения последнего хорошего конфига в etcd или куда-то еще
func (l *AppLoader) loadLastKnownGoodConfig() error {
	f, err := os.Open("last_known_good_config")
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("last known good config does not exist")
		}
		return err
	}
	if err := gob.NewDecoder(f).Decode(l.cfg.App); err != nil {
		return err
	}
	return nil
}

// сохраняет текущий конфиг
// todo абстрагировать для сохранения последнего хорошего конфига в etcd или куда-то еще
func (l *AppLoader) saveConfig() error {
	f, err := os.Create("last_known_good_config")
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(l.cfg.App); err != nil {
		_ = f.Close()
		return err
	}

	_ = f.Close()
	return nil
}

// реализация ConfigProvider
func (l *AppLoader) Config() Config {
	return *l.cfg
}

// BadConfigError означает ошибку в конфиге.
// Возвращать ошибку должен сервис или резолвер fx, который проверяет семантическую корректность значений
type BadConfigError struct {
	Cause error
}

func (e BadConfigError) Error() string {
	return fmt.Sprintf("bad config: %s", e.Cause.Error())
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
			// для удобства в приложении стоит создать такой резолвер
			// и другим резолверам уже передавать конкретный конфиг (как ниже)
			func(appConfig Config) SomeAppConfig {
				cfg, ok := appConfig.App.(*SomeAppConfig)
				if !ok {
					panic("Config does not contain *SomeAppConfig")
				}
				return *cfg
			},
			func(cfg SomeAppConfig, configProvider ConfigProvider) *echoHandler {
				return &echoHandler{
					configProvider: configProvider,
					respTimeout:    cfg.EchoHandler.ResponseTimeout,
				}
			},
			func(cfg SomeAppConfig, handler *echoHandler) (*echoServer, error) {
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

// пример какого-то конфига, специфичного для приложения
type SomeAppConfig struct {
	EchoHandler EchoHandlerConfig `envconfig:"echo_handler" json:"echo_handler"`
	Server      ServerConfig      `envconfig:"server" json:"server"`
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
	configProvider ConfigProvider
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
	appLoader, err := LoadApp("APP", ProvideApp(), new(SomeAppConfig))
	if err != nil {
		panic(err)
	}
	if err := appLoader.Start(context.Background()); err != nil {
		panic(err)
	}
}
