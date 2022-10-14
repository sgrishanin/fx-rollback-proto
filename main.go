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

func main() {
	appLoader, err := LoadApp("APP", ProvideApp(), new(SomeAppConfig))
	if err != nil {
		panic(err)
	}
	if err := appLoader.Start(context.Background()); err != nil {
		panic(err)
	}
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
		return nil, errors.Wrap(err, "failed to create app")
	}

	return &l, nil
}

// здесь содержится основная магия с попытками сборки приложения на разных конфигах
func (l *AppLoader) createApp(cfgPrefix string, appProvider fx.Option, appConfigPtr interface{}) (err error) {
	l.cfg = &Config{
		App: appConfigPtr,
	}
	// сначала грузим конфиги самого загрузчика
	err = l.initLoaderConfigFromEnv()
	if err != nil {
		return errors.Wrap(err, "failed to init loader config")
	}

	// потом делаем попытку загрузить текущий конфиг.
	// на этом этапе может быть либо ошибка парсинга конфига
	if err := l.loadCurrentConfigFromEnv(cfgPrefix); err != nil {
		if _, ok := unwrapBadConfigError(err); !ok {
			return errors.Wrap(err, "failed to load current config from env")
		}

		// если случилась ошибка плохого конфига, пытаемся откатиться

		if err := l.loadLastKnownGoodConfig(); err != nil {
			return errors.Wrap(err, "failed to load fallback config")
		}
	}

	// имея какой-то конфиг, который мы смогли распарсить,
	// пытаемся собрать с ним приложение в fx

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

	// если какой-то из резолверов кинул ошибку, она будет здесь
	err = l.app.Err()

	// если ошибки нет, можем спокойно выходить, предварительно сохранив текущий конфиг
	if err == nil {
		if err := l.saveConfig(); err != nil {
			return errors.Wrap(err, "failed to save current config")
		}
		return nil
	}

	err, ok := unwrapBadConfigError(err)
	if !ok {
		return errors.Wrap(err, "failed to create app with current config")
	}

	// если поняли, что это ошибка плохого конфига, пытаемся откатиться
	// если мы уже откатились ранее (на моменте парсинга выше), будет возвращена ошибка

	configError := err
	if err := l.loadLastKnownGoodConfig(); err != nil {
		return errors.Wrap(err, "failed to fall back to last known good config")
	}
	l.cfg.ConfigError = configError.Error()

	l.app = fx.New(appOptions)
	// если же даже с откатом не получилось запустить приложение - все, приехали
	if err := l.app.Err(); err != nil {
		return errors.Wrap(err, "failed to create app with last known good config")
	}

	if err := l.saveConfig(); err != nil {
		return errors.Wrap(err, "failed to save current config")
	}
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
		parseErr := &envconfig.ParseError{}
		if errors.As(err, &parseErr) {
			return ErrBadConfig{Cause: err}
		}
	}
	return nil
}

// загружает последний известный рабочий конфиг
// todo абстрагировать для сохранения последнего хорошего конфига в etcd или куда-то еще
func (l *AppLoader) loadLastKnownGoodConfig() error {
	if l.cfg.IgnoreFallbackConfig {
		return errors.New("fallback config is ignored")
	}

	if l.cfg.UsesFallbackConfig {
		return errors.New("fallback config is already applied")
	}

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
	l.cfg.UsesFallbackConfig = true
	return nil
}

// сохраняет текущий конфиг
// todo абстрагировать для сохранения последнего хорошего конфига в etcd или куда-то еще
func (l *AppLoader) saveConfig() error {
	if l.cfg.UsesFallbackConfig {
		return nil
	}
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

type ConfigProvider interface {
	Config() Config
}

// реализация ConfigProvider
func (l *AppLoader) Config() Config {
	return *l.cfg
}

func (l *AppLoader) Start(ctx context.Context) error {
	startErr := make(chan error)

	go func() {
		startErr <- l.app.Start(ctx)
	}()

	select {
	case err := <-startErr:
		return err
	case <-l.app.Done():
		return nil
	}
}

// ErrBadConfig означает ошибку в конфиге.
// Возвращать ошибку должен сервис или резолвер fx, который проверяет семантическую корректность значений
type ErrBadConfig struct {
	Cause error
}

func (e ErrBadConfig) Error() string {
	return fmt.Sprintf("bad config: %s", e.Cause.Error())
}

func unwrapBadConfigError(err error) (error, bool) {
	errBadConfig := &ErrBadConfig{}
	if errors.As(err, &errBadConfig) {
		return err, true
	}
	// fx врапает ошибки из резолверов в свои структуры, нужно получить исходную ошибку
	if errBadConfig, ok := dig.RootCause(err).(ErrBadConfig); ok {
		return errBadConfig, true
	}
	return err, false
}

// все что ниже - это пример приложения, которое запускается через AppLoader

// пример какого-то конфига, специфичного для приложения
type SomeAppConfig struct {
	EchoHandler EchoHandlerConfig `envconfig:"echo_handler" json:"echo_handler"`
	Server      ServerConfig      `envconfig:"server" json:"server"`
}

type EchoHandlerConfig struct {
	ResponseTimeout time.Duration `envconfig:"response_timeout" json:"response_timeout"`
}

type ServerConfig struct {
	Host string `envconfig:"host" json:"host"`
	Port int    `envconfig:"port" json:"port"`
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
					return nil, ErrBadConfig{errors.New("server host can't be empty")}
				}
				port := cfg.Server.Port
				if port > 8999 || port < 8000 {
					return nil, ErrBadConfig{errors.New("server port should be between 8000 and 8999")}
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
